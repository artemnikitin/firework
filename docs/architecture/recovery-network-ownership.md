# Network ownership during recovery

**Status:**
- Design for C4 in PR #66 (issue #48). Decisions agreed 2026-11-04. Implemented through the increments below; the final head awaits the contract review.
- **Contract v1.1 is agreed.** It adds two limitations, both of which qualify C5:
  - **L6:** host-port history;
  - **L7:** unconfirmed route withdrawal.

  C4 and L3 are unchanged, and no wrong-service delivery is accepted.
- **C4 is not closed** until the implementation lands and passes review.

## Invariant

> A VM may be started for a service at a guest IP only if every forwarding rule and every route that could deliver traffic to that IP or to a host port it claims belongs to that service.

**What counts as evidence.** Ownership is decided from live evidence and records the agent wrote before acting:
- the kernel's rules;
- the route directory;
- the Traefik process;
- running VMs;
- the journal.

A missing, unreadable or unproven piece of evidence holds the start. It is never read as "nothing there". Holding is a loss of availability, never misrouting.

## Proving that a route is withdrawn

Traefik's file provider gives no acknowledgment. The following was tested against Traefik 3.3.4, the version the deployment installs; the steps are in the appendix.

| Candidate proof | Result |
| --- | --- |
| Remove the route, then wait for a marker route written afterwards to be served on `web`. | **Unsound, reproduced.** A directory scan read route A, the agent removed A and wrote marker `g1`, then the same scan read `g1`. Traefik served `g1` with route A still present for about 2 s, until its next reload. |
| Two rounds: after `g1` is served, write `g2` and wait for it. | Sound only while builds run one at a time, which is true of watch-triggered builds. **Unsound, reproduced:** a build started by SIGHUP read A, then paused. Meanwhile `g1` and then `g2` were served without A. Released, the paused build sent A together with `g2`, and Traefik applied it. The marker cannot reveal that A is back. |
| Routes and marker in one file, written atomically | Each build is consistent. But Traefik applies provider messages in arrival order, so a build that read the old file can arrive after a newer one and bring A back. The SIGHUP case above shows that late arrival happens. |
| Traefik's API | Not reliable from source: the API is built from the same runtime configuration, but entrypoints switch one at a time (`TCPEntryPoints.Switch`), so it can report a configuration before `web` serves it. The deployment has no API enabled. |

**Why none of these works.** Builds are not serialized. In Traefik 3.3.4 the startup build, the watcher goroutine and the SIGHUP goroutine all call `applyConfiguration` independently (`pkg/provider/file/file.go`). The configuration watcher keeps whichever message arrives last. So a configuration Traefik has served proves nothing about what it will serve later.

**The proof used instead:** no Traefik process that could hold the old route is still running, and every process that starts later reads the directory after the route is durably gone. The proof is needed only when an address moves to a **different** service. A stale route for X that still points at X's new VM at the same address delivers X's traffic to X.

**Withdrawal sequence.** Each step is fsynced before the next:

1. Record `{service, guest_ip, state: withdrawing}`.
2. Rewrite or remove the route file, then fsync the file and the directory.
3. Read the route directory again and confirm that no local route for that service points at the IP.
4. Observe the process serving routes now (the boot ID and the unit's `ExecMainStartTimestampMonotonic`), record it, and mark the record `withdrawn`. If no process runs, release the address at once: any later process reads the directory without the route.

**After a crash** between steps 1 and 4, the record stays `withdrawing` and the address stays held. After restart, the agent finishes steps 2 to 4 and observes the process running then, so the proof needs a Traefik start after that.

**Release requires a `withdrawn` record** and one of these, observed later:
- the configured unit (`traefik.service` by default) is inactive or failed and has no main PID;
- its main process has another start timestamp than the one recorded: a process can only have started after the observation, which followed the durable removal;
- another boot ID.

Comparing the observed process identity, rather than times, needs no clock shared with systemd.

**These never release a `withdrawing` record:** a boot change, an inactive unit or a later start. At that point the route file may still exist, and a new process would read it.

**Any other answer is no proof:** a unit that is not loaded, an error, or the recorded process. Status names the unit and the action, and the address stays held.

**Assumptions** (documented with `traefik_unit`):
- The configured unit's main process is the only process serving `traefik_config_dir`.
- It reads only the files present in that directory when it builds a configuration.
- Nothing else keeps firework's routes.

This needs no marker, probe or API. Nothing in the deployment sends Traefik SIGHUP, and the proof does not depend on that.

## Records (journal version 2, `pending_teardowns.json`)

The journal holds authority and history. The kernel and the route directory are the inventory. Every record is fsynced before the action it covers.

| Record | Written | Removed |
| --- | --- | --- |
| **Allocation** `{service, instance_id, guest_ip, host_ports, vm_dir, socket_path, config_path, launcher_unit, phase}`, where `phase` is one of `creating`, `running` or `departing` | `creating`, before anything is created for a new VM: devices, the VM directory, rules. `running` once the VM is started, its manifest is durable and it is tracked. `departing` before the agent stops a VM for removal, replacement or a clear. | Once the VM is proven absent, the inventory shows none of its rules, its devices are gone, and its route record, if any, exists. |
| **Route withdrawal** `{service, guest_ip, state, boot_id, process_start}`. The migration baseline is a withdrawal of every address. | `withdrawing` before the route file changes, and `withdrawn` after the change is durable and verified (sequence above). | When the Traefik proof holds. |
| **Host-port holder** `{host_port → service}` | When an allocation claims the port. | Never (L6). |
| **History** `{complete, initialized_at}` | Once, when the journal is created (see "Initialization"). | Never. |
| **Fences** (not persisted) | Derived from the kernel's rule list and the route directory at every check: a trace no record accounts for holds its address and ports. | `clear-recovery --forget SERVICE` turns the service's tagged traces into departing records; a foreign rule's fence ends when the operator deletes it. |
| **Network devices** | Unchanged. | Unchanged. |

**What an allocation record does:**
- **Authorizes cleanup.** It authorizes deleting the rules tagged for its service and IP, and cleaning up its devices, once the VM is proven absent. The absence proof uses the recorded paths and unit. It does not need the VM directory.
- **Holds while it exists.** In any phase, its IP and host ports are held for every other service.

## Inventory and classification

**Port-forward rules** are installed tagged: `-m comment --comment firework:pf:<service>`.
- The service name is used verbatim if it matches `[A-Za-z0-9._-]{1,128}`; otherwise `h-` and the first 16 hex digits of its SHA-256.
- The inventory is `iptables -t nat -S PREROUTING`, keeping DNAT rules whose destination is in the guest subnet. It is listed once per tick, kept up to date with the agent's own successful changes, and listed again after any iptables error.
- Rules are deleted by their listed text, so teardown no longer depends on the current host IP.
- iptables 1.8.11 (nf_tables) was checked to list a tagged rule with its comment, to treat the untagged spec as a different rule, and to delete a rule exactly by its listed text.

| Trace | Action |
| --- | --- |
| A rule whose exact tuple the tracked VM at that IP claims, with trusted config | Keep it. An untagged rule is adopted: the tagged rule is appended first, then the untagged one is deleted. A rule tagged for another service is re-tagged the same way. |
| A tagged rule toward a tracked, trusted VM, but not claimed by it (an old port, an old host IP or a wrong tag) | Delete it. |
| Any rule toward a quarantined VM | Keep it. The IP is held by the VM in any case. |
| A tagged rule covered by an allocation that may be cleaned up (absence proven) | Delete it. |
| An untagged rule covered by a migrated version-1 entry | Delete it. |
| A tagged rule or a local route that no record covers | Fence its IP and host ports for every service, including the tagged one, and leave the rule in place. |
| Any other untagged rule toward the subnet | **Foreign.** Never deleted. It fences its IP and port, and status names the rule and tells the operator to inspect and delete it. |
| A route file the agent did not name, or cannot parse | Fence its server IPs. If it cannot be parsed at all, every start is held. |
| A Firecracker process whose arguments name a path under `<state_dir>/vms/`, or an active `firework-vm-*` unit, that is not tracked and not covered by an allocation | **Unaccounted VM.** Its IP is unknown, so every allocation of an address not already held by the same service's tracked VM is held. Status names the PID or unit. The hold clears when the process exits. |

## Start gate

`startGate(svc, ip, claims)` holds the start if any of these is true:

- **G1:** the inventory, the route directory, the journal or the unaccounted-VM scan could not be read.
- **G2:** a rule toward `ip` has a tuple outside `claims`. There is no exception for the same service: its own old rules for released ports go first.
- **G3:** a rule for a claimed host port has any other tuple.
- **G4:** a route to `ip` belongs to another service or to an unknown owner, or another service's route withdrawal at `ip` has no Traefik proof.
- **G5:** a fence or a foreign rule covers `ip` or a claimed port, or an unaccounted VM exists (as in the table above).
- **G6:** another service's allocation, in any phase, holds `ip` or a claimed port. Or `svc`'s own `running` allocation has a missing VM directory, which is a quarantine.
- **G7:** a claimed host port's recorded holder is another service, or it has no holder while history is incomplete (L6).

**Where the gate runs:**
1. At plan time, so status shows the hold.
2. Before an update deletes the old VM, with every check except G3. The old VM's own rules and allocation count as going away. G3 also applies when the conflicting port's owner is retained or held, so the old VM keeps running.
3. In `createService`, right before Start, with every check. A hold there is an incomplete outcome; the rolling path continues and the start is retried.
4. Before a health restart stops the VM, so a refused restart leaves it running.

**Holds in status:**
- Holds are transient waits, never `guest_addresses_exhausted`.
- Fences, foreign rules, unaccounted VMs, L6 and L7 name the operator's action. They never suggest that another tick will resolve them.
- `assignNetworking` skips held addresses. It prefers addresses with no history, then the service's own old addresses.

## Lifecycle and crash windows

**Create:**
1. Allocation `creating`.
2. Devices journalled and set up.
3. Start writes the starting manifest, then launches.
4. Tagged rules installed.
5. Allocation `running`.

**After a crash during create**, with the record in `creating`, startup resolves it automatically:
- If the manifest exists, normal recovery adopts or clears the VM, and the record follows.
- If there is no manifest, or the directory is incomplete, the absence proof runs on the recorded paths and unit. Once it succeeds, the tagged rules and devices are torn down, the record goes to `departing` and is then removed.
- The address is then free for the same service at once, and for another service at once too, because no route is published while `creating`.
- A live claimant found by the scans keeps the service quarantined, as in C1.

**A failed start** moves the record to `departing` and tears down, as above. A crash during that teardown resumes from the record.

**A health restart** keeps the `running` record. If the start fails, the record goes to `departing` and the rules stay authorized for deletion, which fixes the failed-health-restart finding.

**Delete or replace:**
1. `departing` is written.
2. The route withdrawal record is written, and the route file is changed.
3. The VM is stopped.
4. Rules and devices are torn down by inventory.
5. The record is removed.

Each step resumes after a crash.

**A VM directory removed by hand.**
- **The record survives.** The allocation lives in the journal, not in the VM directory.
- **The service is quarantined.** A `running` record without its directory quarantines the service (G6), and its IP and ports stay held.
- **Only `--forget` resolves it.** `clear-recovery --forget SERVICE` runs the process and unit scans on the recorded paths and unit, and refuses while the VM is live.
- **Release waits for evidence.** On success, the record goes to `departing`. Release then follows the ordinary path: rules gone in the inventory, devices gone, and a Traefik proof if a route existed and the address moves to another service.
- **If the journal was lost too,** the unaccounted-VM scan still finds the live process and holds every new allocation. Once the VM has stopped, its tagged rules and route files are fenced traces that `--forget` (by the tagged service) turns into teardown.

**Clears without trusted metadata** (an untrusted record, VM state without a manifest, a missing directory):
- **`--forget` is always required.**
- **Prerequisites are mandatory:** the absence proof and the inventory.
- **What the command does:** it moves the allocation to `departing` and turns fences into records that authorize teardown. It never releases an address by itself.
- **Untagged rules** stay foreign until the operator deletes them, which the inventory then shows.

## Cross-node exposure and route history (L6, L7)

**How stale delivery happens.** Peers address a service on this node as `hostIP:hostPort`, in two ways:
- other nodes' Traefik routes;
- `cross_node_links` rendered into guest environments, which stay in place until those VMs are replaced.

If this node gives host port P to another service, stale peer references deliver the old service's traffic to the new one. This node cannot observe when peers or old guest configurations have caught up.

**Rule.** A host port, once allocated to a service on a node, can later be allocated only to that same service on that node, for the lifetime of the node's state (G7). The holder record persists across reboots, because peers' references do too.

**Contract v1.1 limitations.** Both qualify C5: the desired state does not converge on that node until the action is taken, and status names the action.

| ID | Trigger and consequence | Resolution or boundary |
| --- | --- | --- |
| **L6: Host-port history** | A desired state gives a node's host port to a service other than its recorded holder there. It also applies when history is incomplete and the port has no holder. The service waits with `host_port_reuse_blocked` or `network_history_incomplete`. Port swaps between services are unsupported. In control-plane mode the scheduler does not know about holders yet, so a placement can wait there. | **Resolves it:** a fresh port, or a node endpoint without history (see "Initialization"). **Does not resolve it:** convergence, deleting the journal, or replacing the node at the same host IP. Holder-aware placement and a coordinated reset that proves old references are gone are follow-up work. This restricts transitions; it never permits wrong-service delivery. |
| **L7: Unconfirmed route withdrawal** | A service needs a guest address, and the only free ones were withdrawn from another service's route since the running Traefik process started (or before the migration baseline). The service waits with `guest_address_withdrawal_unconfirmed`, which names the unit. | **Resolves it:** the operator restarts Traefik (`systemctl restart traefik`), after which the agent reuses the address. The agent never restarts Traefik. Allocation prefers addresses with no history, so this needs a nearly exhausted subnet or a migrated node. |

**Not chosen:**
- Accepting stale delivery in L3.
- Distributed acknowledgment in this PR. "Every node acknowledged" would also have to cover guests' rendered environments and unreachable nodes.
- The agent restarting Traefik itself.

## Initialization, migration and downgrade

- **Clean initialization (complete history).** It applies when the state directory has no `vms` directory and no journal, the host has no DNAT rule toward the guest subnet, and the route directory has no route file (a local service may be named like a peer route file).
  - **Prerequisite** (documented; the agent cannot verify it): the node's host IP has no previous firework history. That means a new address, or a deployment in which every node and guest that could reference it is also new.
- **Bounded migration (incomplete history).** It applies to anything else without a version 2 journal: a version 1 journal, or VM state without a journal.
  - **What it records:**
    - tracked VMs get `running` records and holders for their ports, and their untagged rules are adopted;
    - version 1 port-forward entries become `departing` records that authorize deleting the matching untagged rules;
    - a baseline withdrawal of every address is recorded at the migration time.
  - **What follows:** history stays incomplete for the lifetime of the node's state. Services keep their current addresses and ports, a new host port is held (L6), and a cross-service address reuse needs a Traefik start after the migration (L7).
  - There is no further historical reconstruction.
- **Corrupt journal.**
  - G1 holds every new allocation. Running VMs are untouched and may restart in place.
  - **Repair:** move the file aside, after which the agent migrates as above with incomplete history. Deleting or moving the journal never makes history complete again, because the `vms` directory remains.
- **Downgrade.**
  - **Supported:** replace the node.
  - **In place:**
    1. Stop the agent and all VMs.
    2. Delete the rules `iptables -t nat -S PREROUTING | grep 'firework:pf:'` lists, by their listed text.
    3. Delete the journal.
    4. Start the old agent.

  An older agent rejects a version 2 journal as corrupt, and it cannot delete tagged rules.

## Decisions (agreed 2026-11-04)

1. **Contract v1.1** with L6 and L7 as above.
2. **Traefik restarts are operator-controlled** (L7). The agent reports `guest_address_withdrawal_unconfirmed`.
3. **New agent setting `traefik_unit`**, defaulting to `traefik.service` when `traefik_config_dir` is set, with the assumptions above.
   - When no proof is available, the affected allocations wait with a reason naming the unit.
   - Docs and examples change; no deployment change is needed.
4. **Journal version 2** with durable allocation records, clean initialization and bounded migration as above.
5. **`--forget` required for every untrusted clear**, including a missing directory.
   - The absence checks stay mandatory, and resources are released only on evidence.
   - This changes `main` and this PR's current automatic clear of state without a manifest.

## Acceptance matrix

Every case goes through failure, recovery and eventual safe reuse. Reproducers from the review of `5392d54` must fail on that commit.

| Case | Expected sequence |
| --- | --- |
| Route file removed, then the agent crashes while Traefik still serves the route | After restart, another service is held at that IP and the same service may reuse it. A Traefik start time before the withdrawal is still held; after a Traefik restart (fake systemd), the address is reused. |
| A Traefik build reads the old route and its result arrives late (SIGHUP or startup) | No marker is involved. Release requires a process other than the one recorded at completion. |
| Crash after the `withdrawing` record, before the file changes, then a reboot that restarts Traefik | Held: a boot change does not release a `withdrawing` record. After restart the agent removes the file and records `withdrawn`. Release needs a Traefik start after that, then the address is reused. |
| Crash after the file change, before `withdrawn` is recorded | The same: held until the agent verifies and records `withdrawn`, then until a later Traefik start. |
| The unit is inactive while a record is `withdrawing` | Held. After `withdrawn` is recorded, an inactive unit releases the address. |
| The Traefik unit is unknown, or systemctl fails | Held, with a reason. Addresses with no history are still allocated. |
| Crash at each create step (before devices, after devices, after the manifest, after launch, after rules) | Recovers automatically after restart: adopted, or absence proven and torn down. Then reuse. Never requires `--forget`. |
| Crash during the cleanup of a failed start, or during a delete or replace step | Resumes from the record. Release only after the inventory shows the rules gone. |
| A live VM's directory removed by hand | Quarantined, IP and ports held; `--forget` refused while live; VM stopped; `--forget` succeeds; teardown; Traefik proof if routed; another service gets the address. |
| A live VM whose directory and journal are both lost | The unaccounted-VM scan holds new allocations. When the VM stops, its traces are fenced; `--forget` by the tagged service clears them; then reuse. |
| A dead VM's directory removed by hand | `--forget` with the directory missing; process and unit scans; teardown; release. |
| A foreign untagged rule toward an IP or a port | Fenced and never deleted. The operator deletes it, the fence clears, and the address or port is reused. |
| Clean initialization | History complete. Fresh ports and addresses are allocated without waiting. |
| Migration with untagged rules and version 1 entries | Rules adopted or torn down; a crash between append and delete converges. Current services keep their ports and addresses. A new host port waits with `network_history_incomplete`. A cross-service address reuse needs a Traefik start after the migration. |
| Corrupt journal moved aside | Migration with incomplete history, never clean initialization. |
| Host IP changed | The new rules are installed beside the old ones, which only match traffic to the old host IP. The old rules are deleted by listed text when the VM's record departs. |
| Host port moves to a different service (including a swap) | `host_port_reuse_blocked` (G7) before the old VM is deleted, and the old VM keeps running. A fresh port converges. |
| Subnet exhausted except for addresses withdrawn from another service | `guest_address_withdrawal_unconfirmed`, naming the unit. After a Traefik restart (fake systemd), the address is reused. |
| The same service retakes its port or IP | Allowed once its own stale rules are gone: `TestReviewSameServiceReuseUnderSubnet`, `TestReviewSameServiceReuseRedirectsAnotherServicesPort`. |
| Failed health restart after rules were installed | `TestReviewFailedHealthRestartLeavesUnjournalledRule`: the record goes to `departing`, the IP is held, teardown runs, then release. |
| Agent restart while a route still points at a removed VM | `TestReviewRestartForgetsPublishedRoute` |
| Static IP while another service's rule, route or record remains | Held by the reconciler-side gate, then started after release. |
| A desired owner's held address, and a hold missing from status (C6) | Transient wait, not exhausted: `TestReviewSharedIndexSkipsOwnersReservedAddress`, `TestReviewDesiredDepartedOwnerReportsExhaustion`, `TestReviewDepartedHoldMissingFromHeldServices`. |
| Failure after the adoption write (C3) | `incomplete`. |
| A reused tuple reconnects after teardown | No delivery to the new VM. Checked on kernel 6.12 and to be repeated on AL2023 in the real-host run. |

## Implementation (PR #66)

| Commit | Increment |
| --- | --- |
| `157c0ea` | C3: post-write recovery failures report `incomplete`. |
| `43d9c17` | Tagged port forwards; teardown from the kernel's rule list. |
| `4037a75`, `8bf052f` | Journal version 2 with allocation records, crash recovery, clean initialization only on a host without traces, bounded migration. |
| `b2c4dee` | C6: held-back addresses are transient waits and stay their owner's. |
| `b433bb4` | Restart test with a freshly built VM manager under real systemd. |
| `4900898` | Start gate over the kernel's rules at all four call sites; L6 host-port holders. |
| `4e27a9c` | Route withdrawal records, the Traefik process proof, `traefik_unit`, L7. |
| `d9be837` | `--forget` for every untrusted clear; `vm_state_missing`; the unaccounted-VM scan. |

Differences from the plan:
- **Fences** are derived from live evidence at every check instead of being persisted.
- **The L7 proof** compares process identities instead of timestamps.
- **A missing VM directory** is cleared without `--forget` only when the VM manager removed it itself in this process (a failed start or a clear racing an inventory). Otherwise it keeps its fence.
- **Rules under an old host IP** stay until the VM's record departs.

## Increments

One commit each, tests first:

1. **C3:** paths after the adoption write report `incomplete`. This is independent and can land now.
2. **C6:** gate holds are transient for exhaustion and appear in plan-time holds.
3. **Journal version 2:** allocation records, phases and crash windows, clean initialization and bounded migration. The crash cases are checked here, before the later increments build on them.
4. **Inventory:** tagged rules, deletion by listed text, adoption, fences and foreign rules.
5. **Gate:** the gate at its four call sites, host-port holders and L6.
6. **Routes:** the withdrawal sequence and records, the Traefik proof, L7, and `traefik_unit` with its docs and examples.
7. **Clears:** `--forget` for untrusted clears and the unaccounted-VM scan. Then `DESIGN.md`, the contract text, the behaviour changes, and the PR and issue descriptions.

**After the increments:**
- One contract review of the final SHA.
- The real-host run, which gains three checks: tagged rules on AL2023, the Traefik restart proof against the deployed unit, and the conntrack repeat.

**Out of scope:**
- holder-aware placement and a coordinated host-port reset;
- allocation optimization;
- an inventory of TAP devices beyond the device journal.

## Appendix: experiments

All runs used throwaway containers on Docker Desktop (kernel 6.12.76-linuxkit), which were removed afterwards.

**Paused directory scan (one-round marker).**
- **Setup:** Traefik 3.3.4 with `--providers.file.directory=/dyn --providers.file.watch=true`.
  - `/dyn/a-route.yaml` routes `a.example` to `noop@internal` with a header, so the route answers 418 when present and 404 when absent.
  - `/dyn/zz-firework-generation.yaml` routes `firework-generation.invalid` to `noop@internal` with `X-Firework-Generation`.
- **Pause:** a FIFO hard-linked as `/dyn/m-pause.yaml` makes the next scan block after reading `a-route.yaml`. The link is then removed, and the blocked scan keeps the inode open.
- **Sequence:** remove `a-route.yaml`, write `g1` (temporary file, then rename), release the FIFO with valid YAML.
- **Result:** `g1` served with route A present for 19 polls 0.1 s apart, then A absent.

**Paused SIGHUP build (two-round marker).**
- **Pause:** the FIFO is placed in `/dyn/sub/`. Subdirectories are loaded but not watched, so watch-triggered builds are unaffected. `kill -HUP 1` starts a build that blocks after reading `a-route.yaml`.
- **Sequence:** remove the FIFO link and `a-route.yaml`, write `g1` and wait for it, write `g2` and wait for it. Route A is absent at both points. Then release the FIFO.
- **Result:** `g2` stayed served without A for 18 polls (Traefik's 2 s reload throttle). Then `g2` was served with route A present for all 42 remaining polls. The Traefik log shows the late build's configuration containing both A and `g2`.

**Stale conntrack entry.**
- **Setup:** a client connects through a tagged DNAT rule to VM A.
- **Sequence:** both ends drop silently. The rule is deleted, A's namespace is deleted, and B takes the address and listens.
- **Result:** a SYN reusing the exact tuple reached the host untranslated and was refused. B's counter for that port stayed at 0, and the stale conntrack entry was still listed.
