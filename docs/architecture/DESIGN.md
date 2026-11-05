# Firework Design Decisions

This document explains the key architectural choices behind Firework: what was considered,
what was rejected, and why the current design is the way it is. It's intended for anyone
who wants to understand the tradeoffs rather than just the mechanics.

## Why Firecracker

Firecracker provides VM-level isolation with startup times and resource overhead close to
containers. The original use case for Firework was multi-tenant workloads where container
isolation is not sufficient: different tenants running the same software stack on shared infrastructure, with a hard isolation boundary between them.

Kubernetes with namespaces solves the scheduling and lifecycle problem, but the isolation
boundary is the container, not the VM. Firecracker gives you both: the operational
simplicity of a process + the isolation guarantee of a VM.

The tradeoff is that Firecracker requires `/dev/kvm`. AWS supports this on
bare-metal instances and on supported Intel virtual instances with
[nested virtualization enabled](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html).
The deployment example defaults to `c8i.2xlarge` with nested virtualization;
bare-metal Graviton remains an option for ARM64 hosts. GCP requires a machine
type and image configuration that support nested virtualization. Host kernels
and guest images must match the node architecture.

## Pull-Based Reconciliation

Firework agents pull the desired state rather than receiving it pushed from a controller.

The alternative (central controller that pushes config to each node) introduces a
stateful coordination problem: the controller needs to track which nodes have received
which config, handle retries, deal with nodes that are temporarily unreachable, and
maintain a consistent view of the cluster state. That's a significant operational burden.

Pull-based design means:
- Nodes are self-healing by default. An agent that crashes, restarts, or misses a config
  update will converge on the next poll.
- The control plane is stateless from the node's perspective. It writes config
  to object storage and forgets about it.
- Nodes can be added or removed without coordinating with a controller.

The cost is eventual consistency and a polling delay (30s by default). 

## Role-Based Control Plane

The enrichment and scheduling problems are meaningfully different, but we still ship
them as a single control-plane binary. The runtime is split by role:

**Events** is a pure transformation trigger: take GitHub push input, clone config,
apply defaults/validation, publish desired revision. It's deterministic given the
same input. 

**Controller** is an optimization loop: given a desired revision and live nodes,
find placement that respects constraints and publish rendered node configs.

**Registry** is the node-lifecycle API: enrollment, register, heartbeat, state transitions.

**API** is a read-only projection: join desired state, placement, registry
leases, and bounded agent snapshots into authenticated node and service views.
It never participates in reconciliation and needs only object-storage read
permissions.

This gives operational flexibility (you can run roles separately) without forcing a
multi-service deployment model.

## Node Discovery via Registry Leases

The controller needs to know which nodes exist and what capacity they have.
Node discovery is based on explicit registry records with lease freshness:

1. Nodes enroll with mTLS certs and register identity/capacity.
2. Nodes send periodic heartbeats with current capacity/usage.
3. Controller schedules only nodes with `state=ready` and non-expired lease,
   and places a service only on nodes whose registered labels include its
   `node_type`.

Every lifecycle mutation is fenced by the agent's process generation (a
wall-clock value proposed at agent start). Registration is the only request
that creates a record or advances its generation; a strictly newer generation
replaces the record, an equal one is idempotent, and an older one is rejected.
Heartbeat and state requests must name the record's exact generation and
never create a record:

| Response | Meaning | Agent behavior |
| --- | --- | --- |
| `404 node_not_registered` | No record exists for this node | One registration attempt on the next sync |
| `409 stale_generation` | The record belongs to another incarnation | One registration attempt; if that also conflicts, the agent is terminally `fenced` |

A fenced agent stops all registry traffic, including its shutdown `down`
update, and exposes `firework_agent_registry_fenced 1`. Restarting the
intended agent proposes a new generation and normally recovers. A restart
cannot recover when the host clock is behind the stored generation, because
the new proposal is still older; once the old process is confirmed gone,
delete `cp/v1/registry/nodes/<node>.json` and restart the agent. This is what
prevents an old process shutting down late from marking its replacement
`down`. Registry errors share one envelope:
`{"error":{"code":"<stable code>","message":"<text>"}}`; clients branch on the
HTTP status and `code`, never on the message.

The agent and control plane in this repository must be deployed together:
an agent built before generation-fenced state requests cannot mark itself
`down`, and its heartbeats cannot recreate a missing record.

Registry state is persisted in S3 or GCS objects under
`cp/v1/registry/nodes/*.json`.
This keeps discovery semantics cloud-agnostic.

## Stable Network Assignment

Guest IPs, MACs, and TAP device names are not stored in any database. A service whose
VM the agent tracks keeps that VM's address, whatever its state: running, failed,
stopped, restarting, or quarantined. Every other service takes the lowest free
address, in service-name order.

This means:
- No separate network allocation store; addresses come from VM ownership records
- Removing a service or a crash does not shift the addresses of existing VMs
- A fresh node with no VM state assigns addresses in service-name order
- Existing assignments are restored before any new one is made; a new service
  that finds no free address is held with `resource_held` instead of being
  reconciled without networking

The implication is that the layout depends on history: the same set of services can hold
different addresses on two nodes, and a service created after a removal can take the freed
address.

## Static Service Linking

Services that depend on each other (e.g. UI → backend) are linked while desired
state is resolved, not through a runtime discovery service. The agent resolves
same-node links after assigning guest IPs; the controller resolves cross-node
links after scheduling. The resulting addresses are injected as environment
variables into the dependent service's kernel args.

The alternative — runtime service discovery (DNS, Consul, etc.) — requires running
additional infrastructure inside the VM network, adds latency to startup, and introduces
a dependency on a service that must itself be highly available.

Static linking is simpler and eliminates the runtime dependency entirely. The
constraint is that service addresses are fixed during each reconciliation and
are not looked up dynamically by the guest. 

Environment injection uses kernel args (`firework.env.KEY=VALUE`, or
`firework.env64.KEY=VALUE` for values that need whitespace encoding), which the guest
init process (`fc-init`) reads from `/proc/cmdline` and exports to the environment
before exec-ing the application. This means no sidecar, no agent inside the VM, and
no dependency on the guest OS beyond a standard init binary.

## Traefik with File Provider for Dynamic Routing

Each node runs Traefik as a reverse proxy. Firework writes Traefik dynamic config files
(one per service) that Traefik watches and reloads automatically.

The alternative: managing cloud load-balancer routing rules programmatically. It would require the control plane to call provider APIs after scheduling changes,
handle rule limits, and manage ordering. Traefik with file provider is simpler:
the agent writes a file, Traefik picks it up, and no provider API calls are
needed.

A service requests a public route via its metadata: `metadata.subdomain` (a single DNS
label, resolved to `<subdomain>.<ingress_domain>` using the agent's deployment-owned
`ingress_domain`) or `metadata.host` (an exact hostname used verbatim, retained for
backward compatibility and custom hosts). A single shared resolver turns metadata into the
final hostname, so local and remote routes for the same service are identical regardless of
scheduling. The route set is fully resolved and validated in memory before any file is
written; files are staged and renamed into place so Traefik never observes a partial
document.

Route failures are scoped by fault domain. Invalid metadata on the node's own services
fails the revision, so a bad config is retried rather than recorded as applied. Peer-derived
problems never block local progress: if the peer configs cannot be listed in full, the agent
keeps the existing `remote-*.yaml` files as last-known-good (syncing against an incomplete
peer set would delete valid routes) and retries on the next poll, and a peer entry that
cannot be rendered is skipped with a warning. Hostname conflicts resolve deterministically —
local services first, then peers in node-name order — which makes the transient reschedule
window (where a service briefly appears in both a stale peer config and its new node's
config) converge without cluster-wide errors. Genuine duplicates are rejected earlier by
enricher input validation.

For multi-node routing, agents using an object-storage-backed config store read
configs for all peer nodes and write `remote-{svc}.yaml` files for peer services
that have a routing key (`metadata.subdomain` or `metadata.host`) and a
`port_forwards` entry. Those files proxy to the peer node's `host_ip` and
forwarded host port. This means every node can route to every routed service in
the cluster, which avoids a load balancer sending requests to a node that does
not have the target service scheduled.

Traefik was chosen primarily for integration simplicity — since the agent already manages
files on disk, a proxy that watches config files required no additional API surface compared
to alternatives like nginx or Envoy.

## VM Process Isolation

On Linux hosts where the agent runs as root under systemd, each Firecracker
process is launched as a transient `firework-vm-<instance-id>.service` in
`firework-vms.slice`. This keeps the VM outside the agent service's process
lifecycle. Local development, non-systemd hosts, and unprivileged agents use a
separate process group instead, because creating a system-scope transient unit
as a non-root user is refused by polkit. The launcher actually used is recorded
in the manifest, so stopping and adopting a VM never depends on re-deriving it.

Before launch, the agent atomically writes
`<state_dir>/vms/<service>/instance.json`. The manifest records the resolved
service config and hash, a unique Firecracker `--id`, launcher details, and a
process identity composed of the host boot ID, PID start ticks, executable
device/inode, and exact command-line paths. On restart the agent validates all
of those fields plus the API socket before adopting a survivor. It then
re-establishes idempotent TAP, forwarding, and health-monitor state without
restarting a VM whose resolved configuration is unchanged.

The identity is recorded only once the launched PID is running the command line
Firework launched for it. systemd reports a transient unit's `MainPID` at fork,
so a PID inspected any earlier is still systemd, and a manifest written from
that inspection describes a process that never existed at that identity and can
never be validated again. A launch whose identity cannot be confirmed within a
short bound is killed rather than recorded; its state directory is removed once
the process is proven gone. If it cannot be proven gone, it is retained as
`recovery_pending` and keeps ownership of its host networking so reconciliation
cannot tear resources down around a potentially live microVM.

Recovery inspects untracked and quarantined runtime directories on reconciliation
ticks. Services already owned in a nonquarantined state are skipped. A shared
five-second pass budget and rotating service order bound retries and prevent one
slow service from starving the others.

A manifest whose recorded identity does not validate is re-proved from the live
command line before it is quarantined. The instance ID is 128 bits generated
once per launch and passed to Firecracker as `--id`, so a process presenting it
together with that instance's socket and config paths is the process the
manifest was written for, whatever the recorded identity says. The identity is
then repaired from the live process and persisted. A recorded process start time
that disagrees is never repaired: that is the PID-reuse signal the manifest
exists to catch.

VM state without an ownership manifest, such as a directory written before
manifests existed, records neither a process identity nor a configuration, so
it is never adopted, whatever process presents its paths and whether or not its
service is desired. Startup recovery, automatic retry and the command all
clear it after a complete absence proof and otherwise keep it quarantined;
once its process is stopped, recovery clears the state and the service is
recreated.

Dead processes have their stale runtime directory cleaned. A host boot ID that
disagrees with the manifest is one of them rather than an ambiguity: the boot ID
is host-global and read fresh on every inspection, so a mismatch proves the
recorded PID cannot be the recorded process. An identity that was never recorded
is reported distinctly from one that disagrees, because an absent field proves
nothing about the process and must not license cleaning up under a VM that may
still be alive.

Ambiguous cases — including PID reuse, mismatched command lines, corrupt
manifests, and a live process with a missing or invalid socket — enter
`recovery_pending`. Firework does not signal the process, delete its files, or
launch a duplicate in that state. Agent status exposes the condition and the
recovery log names the supported command.

Run `firework-agent clear-recovery SERVICE` on the node as root or as the agent's
Unix user. Pass `--config PATH` when the agent uses a nondefault config file.
The command connects to `<state_dir>/control/recovery.sock`, which the agent
creates with a private directory and owner-only socket permissions. The command
rechecks the named service through the running manager, so memory and the
ownership manifest change together. When ownership cannot be proved, the
command changes nothing and exits with an error naming why the service remains
`recovery_pending`, such as a live process still using the VM's paths, and
saying to resolve that and run the command again. If ownership is settled but
recording or removing state then fails, the command reports an incomplete
resolution instead: part of the state may already have changed, the service
stays `recovery_pending`, and the agent retries the rest.

The command re-adopts a VM only when the manifest, config hash, process identity,
launch arguments, and API socket validate. Recovery retries transient failures
on later reconciliation ticks, including when desired configuration is unchanged.
A fully validated live process does not require an additional systemctl query.

Cleanup first attempts a complete absence proof, independently of whether the
old PID/identity is populated. Systemd-owned state requires an inactive or absent
unit with no MainPID and no queued job: `systemd-run --no-block` can leave a
queued unit with no process yet. Where the agent launches through systemd, or
the record names a unit, no `firework-vm-*` unit may name either VM path in its
command line, which covers metadata too damaged to name its unit. All cleanup
also requires no live process referencing either VM path. This permits dead PID-0 starts and unused legacy directories to recover
without hand-editing files. Unknown, unreadable, or live ownership remains pending.
A missing identity cannot authorize live adoption.

Recovery classifies actual process ownership independently of the recorded
lifecycle label: proven absence permits cleanup, a live owned process with a
valid API socket permits adoption, and uncertain ownership stays quarantined.
This also applies to interrupted `stopping` records. A stop whose owned process
outlives SIGKILL enters `recovery_pending` with its `stopping` record, so later
ticks re-adopt the live owner and plan the stop again, or clear it after exit.
The optional `--forget`
flag restricts the command to absence-verified cleanup; it never adopts or kills
a live process. Neither cleanup nor adoption proves guest quiesce or filesystem
consistency. Issue #49's offline filesystem/resize validation remains independent.

If proof fails, inspect the VM logs, ownership manifest, and recorded systemd
unit. Stop/fence the actual owner before retrying. For corrupt metadata, preserve
it for diagnosis and restore a trusted ownership record before retrying; the
agent must not guess a new owner. A restored record is used on the next retry
without restarting the agent. Untrusted metadata (an unreadable or mismatched
manifest, VM state without a manifest, a symlinked service entry, or a
directory that went missing) never authorizes adoption. Its absence proof uses
only the service's canonical paths. Recovery never clears such state on its
own, even once absence is proven, because nothing in it identifies what a dead
owner left on the host. It stays fenced until an operator restores a trusted
record or runs `--forget`, which deletes it after the same proof; for a
symlinked entry, only the link is removed. The exception is state that an
operation of the agent's own explains. Before the manager creates or removes a
VM directory it durably writes an intent marker, `vms/.firework-intent-<service>`,
and removes it once the directory has its ownership manifest or is gone. A
directory without a manifest, or a missing directory, that has a marker is an
interrupted create or removal: recovery clears it after the absence proof,
across agent restarts. A missing directory the agent removed itself in this
process, such as a failed start racing an inventory, is cleared the same way.
Without such evidence, state without a manifest stays quarantined at startup,
on retry and for a plain `clear-recovery`, even when no process uses its
paths; only `--forget` clears it.

Removing a VM's directory by hand never releases anything either. The agent's
ownership records outlive the directory: the service is held with
`vm_state_missing`, and its address and host ports stay fenced. `--forget`
then proves from the service's paths that no process or systemd launch uses
them, and hands the service's records and the rules tagged for it that nothing
accounts for to cleanup. A refused `--forget` changes nothing. The command
itself releases nothing: an address is released once its rules are gone and,
for a route, once Traefik has restarted, and host ports keep their history (see
"Network ownership" below). A live VM process or systemd launch under the state
directory that nothing tracks, as when a directory and the journal were both
removed, holds every new address with `unaccounted_vm` until it stops.

Rules the agent did not install are never deleted. Before running `--forget`,
unless the host has rebooted since the VM ran, find any untagged port forward
and any TAP the old VM used and remove them by hand: an untagged rule toward a
guest address or a host port holds starts there, naming the rule, until it is
gone. On a host that ran an agent from before ownership manifests or tagged
rules, check for such leftovers too. Stopping the agent does not stop surviving
VMs. Do not delete ownership records while a claimant may be alive.

A process observed mid-exit is classified as exited, not ambiguous. Linux
releases a process's address space, and with it its `exe` link and command line,
before the task reaches the zombie state, so identity reads can legitimately
come back partial. Firework treats that as an exit only after confirming a
missing `/proc` entry or a dead process state within a short bound, which keeps
a normally stopping VM out of `recovery_pending` without ever reporting a live
process as gone.

## What Was Left Out

Several things were considered and deliberately not implemented in the initial version:

**Health-gated rolling updates / canary deployments**: The agent supports a
`rolling` update strategy that applies service updates one at a time with an
optional delay. It is still stop-then-start for each updated service: it does
not wait for health before continuing, create surge capacity, or implement
canary traffic shifting.

**Automatic rollback**: A failed reconciliation leaves partial state; the system converges
on the next tick rather than rolling back. Adding rollback would require snapshotting
desired state before apply and tracking which operations succeeded — significant complexity
for a marginal benefit given the pull-based self-healing loop.

**Autoscaling**: Node count is managed externally, for example by an AWS Auto
Scaling Group or a GCP managed instance group. Firework does not scale the node
fleet — it only schedules services onto existing nodes. Autoscaling would
require the scheduler to reason about provisioning latency, node startup costs,
and fleet topology, which is a separate problem.

**Service mesh / workload mTLS**: Communication between services is unencrypted
within the VM network. Networked VMs on a host currently share the Firework
bridge, and the scheduler does not yet enforce tenant-to-node separation. Any
deployment that requires network-level tenant isolation must provide additional
controls. Adding workload mTLS would require a sidecar or library inside each
VM.

### Recovery policy across restart and explicit retry

Restart and the operator command share the same resolver. Start-time reclamation
uses the same absence proof, bound to the requested service's canonical runtime
directory. Quarantined status exposes the directory's service name even if a
malformed manifest names another service. Healthy startup verification does not
emit an operator-intervention error.

A quarantined direct child's exit remains eligible for recovery on the next
tick or explicit retry. Its Wait monitor records the exit without clearing
`recovery_pending`; the resolver still requires the full absence proof before
removing runtime state. The old lifecycle marker does not prevent automatic
cleanup once absence is proven.

Process and unit inspections run outside the global manager lock. Final mutation
revalidates the captured instance and durable record. Restricted process tables
are accepted for root or the configured exempt group; an unprivileged nonexempt
viewer cannot prove absence. An unreadable process during a strict scan still
prevents deletion. The runtime assumes the agent runs in the host PID namespace.

Boot ID, start time, executable inode/device and launch arguments establish
ownership. A `/proc/PID/exe` display-path ` (deleted)` suffix does not invalidate
an otherwise proven process.

Quarantined services are excluded from both rolling and all-at-once plans. They
retain their individual `vm_recovery_pending` status while independent services
continue applying changes. A successfully executed plan advances the node's
applied revision; this acknowledges processing the configuration, not health or
convergence of every VM. Per-service status remains authoritative for readiness.
Successful image synchronization is cached by nonempty store revision, so retrying
quarantine does not repeat the completed image stage. Failed downloads remain
eligible for retry; multi-label inputs without a combined revision still sync.

Manager lock acquisition and commit checks have a five-second budget, shorter
than the transport deadline. A lost response means its outcome is unknown;
inspect status before retrying. Clearing an absent removed VM also deregisters
its health probe on the main loop.


Recovery discovery reserves a service name before inspecting old files. Start
and Remove cannot overtake that inspection. Start preparation does not create a
VM directory; prelaunch failures discard only files belonging to that attempt.
An abort during preparation marks the attempt and leaves any old runtime state
intact. Final removal checks the same instance that was originally selected.

Before recovery deletes VM state with trusted metadata, it marks the VM's
ownership records departing in the reconciler's ownership journal. There is no
separate recovery outbox. Physical cleanup never deletes a rule a tracked VM
claims, so resources retained by a live or quarantined VM are preserved. When
host network setup is disabled, recovery has no network cleanup obligation. A
journal write failure preserves the VM directory for a later retry.

Discovery publishes every persisted VM directory before spending the inspection
budget. Valid ownership records reserve their old CPU, memory, guest IP/MAC,
host-port and host-link claims. Unknown resource metadata holds new
allocations (creates and updates) until its record is forgotten or replaced by
a trusted one. Deletes still proceed, but leave their host cleanup in the
journal until then. Running services that need no change keep their networking, health
checks and port forwards. Conflicting desired services are
held with `resource_held`; independent actions continue. A VM whose update is
held keeps its actual guest IP, MAC, host ports and devices, whether it is
running or has crashed or stopped, since its DNAT rules stay installed until it
is replaced; a running one's rules are re-asserted every tick. Other services
that need any of those claims are held too, and inherit the cause of the
owner's hold. A new service also waits while another tracked VM still uses one
of its claims, for example until a later rolling update moves that VM off the
port. Both update strategies run deletes before creates and updates, so a
service replaced by one that takes its port moves in a single tick.

Holds are classified the same way whether or not any other VM happens to be
retained:

- Recovery holds, caused by unresolved VM ownership (unknown claims, a resource
  retained by a `recovery_pending` owner directly or through a held VM, a guest
  address or MAC an undesired quarantined VM, or one whose removal recovery
  holds, keeps): when every hold is one,
  the agent applies the revision with those holds and reports
  `VMRecoveryReady=False`, so the node shows as degraded instead of
  progressing. Pending recovery keeps forcing reconciliation, so held work
  retries on an unchanged revision. The revision applies this way only if,
  on the runtime the apply left, no desired service needs capacity its VM does
  not already use; a desired quarantined VM whose configuration is unknown may
  need any amount, so at most one is allowed and only if its whole desired
  size fits beside what the node uses besides undesired quarantined VMs. A
  capacity wait is never a recovery
  hold: whether it outlasts recovery depends on admission order, claims and
  how each quarantine resolves. A service recovery holds that needs capacity
  is held for capacity too, and the revision stays incomplete.
- Admission failures, which waiting cannot resolve: desired services whose
  combined CPU or memory exceeds node capacity, or more networked desired
  services than `vm_subnet` has guest addresses. The other services still
  converge as far as admission in service-name order, after VMs that are
  starting, stopping or quarantined, and the claims of held VMs allow, but the
  revision is not applied: `CapacityReady`
  is false with `capacity_exceeded` or `guest_addresses_exhausted`, and the node
  reports failed.
- Transient holds: waiting on another start or stop, on a claim that a running
  or retained VM still uses, on capacity that a removal or shrink this tick, or
  a VM starting or stopping, will free, on capacity quarantined VMs keep when
  everything would fit once they and the work recovery holds resolved, or on a
  guest address an undesired VM keeps until the plan removes it. The revision
  stays incomplete, `VMsReconciled` is unknown with `resource_held`, and the
  next tick retries.
- Capacity that nothing pending frees and recovery does not explain, such as
  two VMs trading sizes on a full node, or a growth waiting for a shrink that
  another hold keeps from running. The revision stays incomplete in the same
  way, and the hold's reason asks for headroom or a staged change: admission
  does not search for an update order that avoids the wait.

Capacity admission never counts on a change before it succeeds: a create or
growth may take only capacity that is free when it runs. Every VM that still
uses its resources (running, starting, stopping or quarantined) counts at its
current size, including one the plan removes or shrinks and one held for any
reason; a failed or stopped VM uses nothing. In service-name order, each create
or update is admitted by what it adds to each resource, and only the resources
it grows are checked, so a shrink, a change that keeps the size, and growth of a
resource the node is not over in proceed even on a node already over capacity.
The agent does not evict anything to repair such excess. A removal or shrink
frees its capacity for the next tick: replacing a service on a full node takes
two ticks, and two VMs trading sizes on a full node need headroom, or a desired
configuration staged so that one shrinks first. Capacity and claim holds are
computed together, from one snapshot of the runtime, starting every tick from no
capacity holds. Within a tick a capacity hold is never released, because the
held VM keeps its claims and can hold others: later passes keep it and admit the
rest around it, which can hold more than the node needs until the next tick.

The reconciler checks every start again when it runs, against the runtime as it
is then: each other VM counts at what it uses, or at the size admission allowed
it if larger, so work admitted earlier keeps its share. A hold that clears
during the apply, a VM that changes state after admission, a rolling apply that
stops at a failing update, an update whose stop or start fails, or a VM
quarantined while the tick runs therefore cannot make room for a start that
admission did not count; such a start waits with a transient hold. A health
restart of a failed or stopped VM takes its resources again and passes the same
check; one that does not fit is skipped, and the next tick reconciles the
service. Restarting a running VM keeps its size. While a quarantined VM's
configuration is unknown, its size is too, so no start may grow anything. With
capacity checks disabled, or before the node's capacity has been read, nothing
is checked.
Running guests retain their assigned addresses after a survivor clears.

Each tick's Recover/Plan/Apply section and health-driven Stop/Start transactions
are serialized; config fetch, image sync, route refresh and the pauses between
rolling updates do not delay a restart. Each rolling update re-checks its hold
and target after a pause, so a restart that ran meanwhile is neither undone nor
repeated. A health restart of an existing VM reuses its prepared volumes and
never runs mkfs or resize2fs; creating a new service's volume inside a tick can
still delay a restart by that preparation. A failed
start-time cleanup retains retryable quarantine instead of dropping the previous
instance. A health restart whose Start fails later forces the next tick to
reconcile, so an unchanged revision still recreates the service. Confirmed exited systemd owners use their unit's terminal state for
normal restart; deactivating or unreadable units remain retryable. Cleanup
journal work also triggers reconciliation on an unchanged revision.

Capacity admission reserves desired growth for retained service names as well
as new services: ownership may resolve between admission and planning in the
same tick. Capacity holds persist through that transition, and the start is
checked again when it runs, so adoption or cleanup cannot authorize an
allocation that skipped the capacity check.

The opt-in `scripts/systemd-recovery/run.sh` validation runs real systemd units,
Linux process inspection and TAP ownership in a disposable container. It also
confirms that recreating a same-name TAP cannot reconnect an old descriptor.
See its README for prerequisites and the simulated-VMM validation boundary.

Local Traefik routes are published from the configuration of the VM that
actually owns each desired service's backend, never from an allocation a hold may
still prevent. Every tracked VM keeps its reserved guest address whatever its
state, so its route cannot reach another guest; a VM whose configuration is
unknown, and a service with no VM, publish nothing. Routes are synced after
every reconciliation, whatever it returned, because deletes and replacements
run even while other work is held or failing: a route never outlives its guest
or points at an address another service now owns. The unchanged-revision path
refreshes them on every poll as before. A guest address is not given to
another service while a removed VM's ownership record or host port rules for
it remain, while a route file still points at it, or while a route withdrawn
from it may still be served; the reconciler also holds any start on such an
address. The service that owned the address may take it back once its own
earlier rules for ports it no longer claims are gone. See "Network ownership".

## Network ownership

Contract C4 and limitations L6 and L7. The design, with the experiments behind
it, is in [recovery-network-ownership.md](recovery-network-ownership.md).

- **Ownership journal.** `<state_dir>/pending_teardowns.json` (version 2)
  records every networked VM before anything is created for it: its guest
  address and port forwards, moving `creating`, `running`, `departing`. A
  record is removed only once the kernel's rule list shows none of its rules.
  A crash at any step recovers without an operator: an interrupted start whose
  VM is not tracked keeps its address until the VM manager proves no process
  or systemd launch uses its paths.
- **Tagged rules, the kernel as inventory.** Port forwards carry
  `firework:pf:<service>`. Teardown lists `nat PREROUTING` and deletes rules by
  their listed arguments, so rules from an old host IP or an older agent go
  too. A rule a tracked VM claims is never deleted. Untagged duplicates of a
  running VM's rules are removed once its tagged rule is installed.
- **Start gate.** At plan time, before an update removes the old VM, right
  before Start, and before a health restart, a start is held while any rule
  toward its address or for one of its host ports is not one of its own
  claimed forwards (no exception for the service's own earlier rules), while
  another service's record, route or unconfirmed route withdrawal holds the
  address, or while the rules or route files cannot be read. A start held
  after its old VM was removed does not stop a rolling update.
- **L6: host-port history.** A host port, once given to a service on a node,
  goes only to that service there, because peers' routes and guests'
  `cross_node_links` may still send its traffic to it. A desired state that
  gives it to another service, including a port swap between services, fails
  the revision with `host_port_reuse_blocked`: change the configuration to a
  fresh port, or use a node without this history. Waiting does not resolve
  it.
- **L7: route withdrawal.** A route is recorded as withdrawn before its file
  changes, and completed once the change is durable and verified, with the
  identity of the Traefik process running then. Its address goes to another
  service only once that process is gone: the `traefik_unit` (default
  `traefik.service`) is stopped, runs another main process, or the host
  rebooted. When only such addresses are free, the agent reports
  `guest_address_withdrawal_unconfirmed` naming the unit and waits for the
  operator to restart Traefik; it never restarts it. `traefik_unit` must be
  the only process serving `traefik_config_dir`. Route ownership applies
  whether or not the agent manages host networking (`enable_network_setup`),
  and routes are told apart by address, not file name: a local service may be
  named like a peer route file.
- **Initialization.** History is complete only when the journal is created on
  a host without a VM state directory, DNAT rules toward the guest subnet or
  route files, on a node endpoint with no earlier firework use. A
  version 1 journal, existing VM state, or a journal moved aside after
  corruption is migrated: running VMs keep their addresses and ports, but a
  port without a recorded holder waits with `network_history_incomplete` (L6),
  and every address waits for a Traefik restart before going to a new service
  (L7). Deleting the journal never makes history complete.
- **Downgrade.** An older agent rejects a version 2 journal and cannot delete
  tagged rules. Replace the node, or stop the agent and every VM, delete the
  rules `iptables -t nat -S PREROUTING | grep firework:pf:` lists by their
  listed text, delete the journal, then start the older agent.

Changes that apply without replacing the VM, such as `metadata.host`,
`metadata.subdomain` or a health-check port, are committed to the running
instance and its ownership manifest during reconciliation unless the service is
held. Routes, health restarts and adoption after an agent restart therefore use
them from the VM's own configuration, and route files carry no extra state. A
held update keeps the previous hostname and backend until its VM is replaced,
but only while desired state does not give that hostname to another service,
local or on a peer: the desired owner always wins, and the stale route is
dropped instead of failing publication for every other service. While the peer
list cannot be fetched, peer claims come from the last successful listing, the
one the preserved remote route files came from; before an agent's first
successful listing, stale routes are dropped because nothing proves their
hostnames free. Peer discovery
runs outside the lifecycle lock; selecting and publishing the local snapshot is
serialized with replacement and health restarts on both tick paths.

Peers build their routes for a service from its desired host port. While that
service's update is held on its owner node, the owner still forwards only the
running VM's actual port, so peer routes to a changed host port fail until the
update lands, as they do during any update window.

Queued-unit absence checks read every `firework-vm-*` unit's state and
`ExecStart` with one `systemctl show` call, which reaches systemd over its
private socket as root and needs no D-Bus daemon. systemctl prints argv joined
by spaces without quoting, so a unit that is not inactive, or has a queued job,
is treated as a potential owner whenever either canonical path appears anywhere
in its properties. Whitespace, quotes and separators in state paths therefore
still match; an unrelated argument containing the path only keeps the entry
quarantined. A state path containing a line break, a potential owner without
`ExecStart`, or a failed systemctl call leaves absence unproved and recovery
pending.
