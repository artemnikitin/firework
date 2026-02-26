# Firework Design Decisions

This document explains the key architectural choices behind Firework — what was considered,
what was rejected, and why the current design is the way it is. It's intended for anyone
who wants to understand the tradeoffs rather than just the mechanics.

## Why Firecracker

Firecracker provides VM-level isolation with startup times and resource overhead close to
containers. The original use case for Firework was multi-tenant workloads where container
isolation is not sufficient — different tenants running the same software stack on shared infrastructure, with a hard isolation boundary between them.

Kubernetes with namespaces solves the scheduling and lifecycle problem, but the isolation
boundary is the container, not the VM. Firecracker gives you both: the operational
simplicity of a process + the isolation guarantee of a VM.

The tradeoff is that Firecracker requires `/dev/kvm`, which on AWS means `.metal` instance
types. That's a meaningful cost and limits the density of workloads compared to
containers on general-purpose instances.

## Pull-Based Reconciliation

Firework agents pull the desired state rather than receiving it pushed from a controller.

The alternative (central controller that pushes config to each node) introduces a
stateful coordination problem: the controller needs to track which nodes have received
which config, handle retries, deal with nodes that are temporarily unreachable, and
maintain a consistent view of the cluster state. That's a significant operational burden.

Pull-based design means:
- Nodes are self-healing by default. An agent that crashes, restarts, or misses a config
  update will converge on the next poll.
- The control plane is stateless from the node's perspective. It writes config to S3 and
  forgets about it.
- Nodes can be added or removed without coordinating with a controller.

The cost is eventual consistency and a polling delay (30s by default). This is an acceptable tradeoff.

## Enricher / Scheduler Split

The enrichment and scheduling problems are meaningfully different:

**Enricher** is a pure transformation: take human-friendly service specs, apply defaults, validate, write resolved per-node configs. It's deterministic given the
same input. 

**Scheduler** is an optimization problem: given a set of services and a set of nodes with
known capacity, find a placement that respects constraints. It needs
to read live node state (capacity metrics) and ideally preserve existing placements.

Combining them into a single Lambda would mix two concerns with different inputs, different
dependencies, and different failure modes. Splitting them means the enricher can run without
the scheduler (for single-node setups), the scheduler can be invoked independently for
debugging, and each can be tested separately.

## Node Discovery via CloudWatch

The scheduler needs to know which nodes exist and what capacity they have. The options are:

1. **Static inventory file** — manually maintained, error-prone, doesn't reflect live state
2. **Some registry/database** — another piece of infrastructure to manage
3. **CloudWatch metrics** — nodes publish capacity metrics; scheduler queries CloudWatch to discover active nodes

CloudWatch is already in the stack for observability. Agents publish `firework_node_vcpus_available`
and `firework_node_memory_available_mb` metrics on each reconciliation cycle. The scheduler
queries CloudWatch `ListMetrics` to discover all nodes that have published recently, and
reads their current capacity.

This avoids a separate node registry entirely. Nodes self-register by publishing metrics,
and they disappear from the scheduler's view when they stop publishing. The tradeoff is up to 15 minutes 
delay before new nodes become visible (CloudWatch metric propagation latency),
which is acceptable given that metal instances take 15-20 minutes to launch anyway.

At larger scale, a dedicated registry would be a cleaner separation of concerns — node
discovery and observability serving different purposes with different consistency
requirements. For the current scale, reusing CloudWatch avoids building and operating
additional infrastructure.

## Deterministic Network Assignment

Guest IPs, MACs, and TAP device names are assigned deterministically from the ordered
list of services, not stored in any database. Specifically, services are sorted by name
before IP allocation, so the same set of services always produces the same assignments.

This means:
- No persistent state needed for network configuration
- Agent restarts or node reboots converge to the same network layout
- Adding or removing a service changes subsequent assignments predictably

The implication is that renaming a service or changing the set of services can shift IP
assignments for other services. It would be a problem for very dynamic workloads.

## Static Service Linking

Services that depend on each other (e.g. UI → backend) are linked at config
resolution time, not at runtime. The enricher resolves the guest IP of the target service
and injects it as an environment variable into the dependent service's kernel args.

The alternative — runtime service discovery (DNS, Consul, etc.) — requires running
additional infrastructure inside the VM network, adds latency to startup, and introduces
a dependency on a service that must itself be highly available.

Static linking is simpler and eliminates the runtime dependency entirely. The constraint
is that service addresses are fixed at enrichment time — suitable for stable, long-running
services, but the wrong fit for workloads where service addresses change frequently.

Environment injection uses kernel args (`firework.env.KEY=VALUE`), which the guest init
process (`fc-init`) reads from `/proc/cmdline` and exports to the environment before
exec-ing the application. This means no sidecar, no agent inside the VM, and no
dependency on the guest OS beyond a standard init binary.

## Traefik with File Provider for Dynamic Routing

Each node runs Traefik as a reverse proxy. Firework writes Traefik dynamic config files
(one per service) that Traefik watches and reloads automatically.

The alternative — managing ALB listener rules programmatically — would require the enricher
to call ALB APIs on each config change, handle rule limits, and manage ordering. Traefik
with file provider is simpler: the agent writes a file, Traefik picks it up, no API calls
needed.

For multi-node routing, agents read S3 configs for all peer nodes and write `remote-{svc}.yaml`
files that proxy to the peer node's host IP. This means every node can route to every
service in the cluster, which avoids the problem of ALB round-robining requests to a node
that doesn't have the target service scheduled.

Traefik was chosen primarily for integration simplicity — since the agent already manages
files on disk, a proxy that watches config files required no additional API surface compared
to alternatives like nginx or Envoy.

## VM Process Isolation

Firecracker VM processes are started with `Setpgid: true`, placing them in their own
process group. This means the agent can restart (crash, update, redeploy) without
sending SIGHUP or SIGTERM to the VM processes it launched.

On restart, the agent reconciles: it reads the current list of running Firecracker
processes, compares against the desired state, and only creates or deletes VMs that have
diverged. VMs that are already in the desired state are left running.

This makes agent updates non-disruptive to running workloads.

## What Was Left Out

Several things were considered and deliberately not implemented in the initial version:

**Rolling updates / canary deployments**: Firework applies all changes in a single
reconciliation pass. There is no built-in mechanism for staged rollouts. 

**Automatic rollback**: A failed reconciliation leaves partial state; the system converges
on the next tick rather than rolling back. Adding rollback would require snapshotting
desired state before apply and tracking which operations succeeded — significant complexity
for a marginal benefit given the pull-based self-healing loop.

**Autoscaling**: Node count is managed externally (ASG desired capacity as in example). Firework
doesn't scale the node fleet — it only schedules services onto existing nodes. Autoscaling
would require the scheduler to reason about provisioning latency, node startup costs, and
fleet topology, which is a separate problem.

**Service mesh / mTLS**: Communication between services is unencrypted within the VM
network. For the current threat model (tenants on separate nodes, intra-node traffic is
isolated to the bridge), this is acceptable. Adding mTLS would require a sidecar or
library inside each VM.
