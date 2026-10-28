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

## Deterministic Network Assignment

Guest IPs, MACs, and TAP device names are assigned deterministically from the ordered
list of services, not stored in any database. Specifically, services are sorted by name
before IP allocation, so the same set of services always produces the same assignments.

This means:
- No persistent state needed for network configuration
- Agent restarts or node reboots converge to the same network layout
- Adding or removing a service changes subsequent assignments predictably

The implication is that renaming a service or changing the set of services can shift IP
assignments for other services. It could be a problem for very dynamic workloads.

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

Recovery is spent once per agent process, including on a fresh node where the
VM state directory does not exist yet. That directory appears as soon as the
process creates its first VM, and a later pass would otherwise "recover" VMs the
running process owns. Services already tracked in memory are skipped for the
same reason.

A manifest whose recorded identity does not validate is re-proved from the live
command line before it is quarantined. The instance ID is 128 bits generated
once per launch and passed to Firecracker as `--id`, so a process presenting it
together with that instance's socket and config paths is the process the
manifest was written for, whatever the recorded identity says. The identity is
then repaired from the live process and persisted. A recorded process start time
that disagrees is never repaired: that is the PID-reuse signal the manifest
exists to catch.

During the first upgrade from the pre-manifest runtime, the agent can migrate
one legacy process when the configured Firecracker executable, resolved desired
service, config path, socket path, and responsive Unix socket all match exactly.
Any ambiguous legacy inventory is quarantined instead of guessed.

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
ownership manifest change together.

The command re-adopts a VM only when the manifest, config hash, process identity,
launch arguments, and API socket validate. Recovery retries transient failures
on later reconciliation ticks, including when desired configuration is unchanged.
A fully validated live process does not require an additional systemctl query.

Cleanup first attempts a complete absence proof, independently of whether the
old PID/identity is populated. Systemd-owned state requires an inactive or absent
unit with no MainPID, and all cleanup requires no live process referencing either
VM path. This permits dead PID-0 starts and unused legacy directories to recover
without hand-editing files. Unknown, unreadable, or live ownership remains pending.
A missing identity cannot authorize live adoption.

An unresolved `stopping` manifest is not adopted automatically. If the VM is
proved absent, use `firework-agent clear-recovery --forget SERVICE` to discard
that runtime marker. This flag still requires the full absence proof; it is not
an assertion that bypasses process checks. It never deletes persistent volumes.
Losing runtime metadata does not prove filesystem consistency or guest quiesce.
When integrated with #49, do not use forgetting as proof that a volume resize is
safe: the offline filesystem/resize validation remains independently required.

If proof fails, inspect the VM logs, ownership manifest, and recorded systemd
unit. Stop/fence the actual owner before retrying. For corrupt metadata, preserve
it for diagnosis and restore a trusted ownership record before retrying; the
agent must not guess a new owner. Stopping the agent does not stop surviving VMs.
Do not delete ownership records while a claimant may be alive.

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

Process and unit inspections run outside the global manager lock. Final mutation
revalidates the captured instance and durable record. Restricted process tables
are accepted for root or the configured exempt group; an unprivileged nonexempt
viewer cannot prove absence. An unreadable process during a strict scan still
prevents deletion. The runtime assumes the agent runs in the host PID namespace.

Boot ID, start time, executable inode/device and launch arguments establish
ownership. A `/proc/PID/exe` display-path ` (deleted)` suffix does not invalidate
an otherwise proven process, including during legacy migration.

Pending VM updates are isolated from independent rolling updates; ordinary
rolling update failures still stop subsequent updates. An unresolved service
keeps node convergence incomplete and the node's applied revision does not
advance. Reporting a node-wide revision as applied would falsely acknowledge
work on the blocked service.

Manager lock acquisition and commit checks have a five-second budget, shorter
than the transport deadline. A lost response means its outcome is unknown;
inspect status before retrying. Clearing an absent removed VM also deregisters
its health probe on the main loop.


Recovery discovery reserves a service name before inspecting old files. Start
and Remove cannot overtake that inspection. Start preparation does not create a
VM directory; prelaunch failures discard only files belonging to that attempt.
An abort during preparation marks the attempt and leaves any old runtime state
intact. Final removal checks the same instance that was originally selected.

Before recovery deletes VM state it atomically stores a validated full manifest
under `<state_dir>/recovery-teardowns/`. The reconciler first copies its old
network resources into the existing durable teardown journal, then acknowledges
the outbox event. Physical cleanup uses the usual running-config claimant guards
so resources reused by a new VM are preserved. Events remain pending during
unresolved/starting/aborting VM state or journal failures. If removal only partly
succeeds and removes `instance.json`, the retained snapshot permits a fresh
absence proof and retry after restart. Multiple incompatible snapshots remain
ambiguous rather than guessing which identity owns the directory.
