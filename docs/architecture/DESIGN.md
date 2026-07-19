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

The tradeoff is that Firecracker requires `/dev/kvm`. On AWS this means `.metal`
instance types; on GCP it requires a machine type and image configuration that
support nested virtualization. That is a meaningful cost and limits workload
density compared with containers on general-purpose instances.

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
3. Controller schedules only nodes with `state=ready` and non-expired lease.

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

On Linux hosts running systemd, each Firecracker process is launched as a
transient `firework-vm-<instance-id>.service` in `firework-vms.slice`. This keeps
the VM outside the agent service's process lifecycle. Local development and
non-systemd hosts use a separate process group instead.

Before launch, the agent atomically writes
`<state_dir>/vms/<service>/instance.json`. The manifest records the resolved
service config and hash, a unique Firecracker `--id`, launcher details, and a
process identity composed of the host boot ID, PID start ticks, executable
device/inode, and exact command-line paths. On restart the agent validates all
of those fields plus the API socket before adopting a survivor. It then
re-establishes idempotent TAP, forwarding, and health-monitor state without
restarting a VM whose resolved configuration is unchanged.

During the first upgrade from the pre-manifest runtime, the agent can migrate
one legacy process when the configured Firecracker executable, resolved desired
service, config path, socket path, and responsive Unix socket all match exactly.
Any ambiguous legacy inventory is quarantined instead of guessed.

Dead processes have their stale runtime directory cleaned. Ambiguous cases —
including PID reuse, mismatched command lines, corrupt manifests, and a live
process with a missing or invalid socket — enter `recovery_pending`. Firework
does not signal the process, delete its files, or launch a duplicate in that
state. The condition is exposed in agent status for operator investigation.

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
