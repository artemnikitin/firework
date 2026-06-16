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

## Role-Based Control Plane

The enrichment and scheduling problems are meaningfully different, but we still ship
them as a single control-plane binary. The runtime is split by role:

**Events** is a pure transformation trigger: take GitHub push input, clone config,
apply defaults/validation, publish desired revision. It's deterministic given the
same input. 

**Controller** is an optimization loop: given a desired revision and live nodes,
find placement that respects constraints and publish rendered node configs.

**Registry** is the node-lifecycle API: enrollment, register, heartbeat, state transitions.

This gives operational flexibility (you can run roles separately) without forcing a
multi-service deployment model.

## Node Discovery via Registry Leases

The controller needs to know which nodes exist and what capacity they have.
Node discovery is based on explicit registry records with lease freshness:

1. Nodes enroll with mTLS certs and register identity/capacity.
2. Nodes send periodic heartbeats with current capacity/usage.
3. Controller schedules only nodes with `state=ready` and non-expired lease.

Registry state is persisted in S3 objects under `cp/v1/registry/nodes/*.json`.
This keeps discovery semantics cloud-agnostic and independent from telemetry pipelines.

Observability metrics remain useful, but they are no longer the source of truth
for control-plane membership decisions.

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

The alternative — managing ALB listener rules programmatically — would require the control
plane to call ALB APIs after scheduling changes, handle rule limits, and manage ordering.
Traefik with file provider is simpler: the agent writes a file, Traefik picks it up, no API
calls needed.

For multi-node routing, agents read S3 configs for all peer nodes and write `remote-{svc}.yaml`
files for peer services that have `metadata.host` and a `port_forwards` entry. Those files
proxy to the peer node's `host_ip` and forwarded host port. This means every node can route
to every routed service in the cluster, which avoids the problem of ALB round-robining
requests to a node that doesn't have the target service scheduled.

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
