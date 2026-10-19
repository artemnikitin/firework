# Deployment visibility

Firework exposes a provider-neutral, read-only deployment API from the
control-plane `api` role. The same process serves a small web UI and the API
consumed by `fireworkctl`.

## API

The API is HTTPS-only and requires a dedicated operator bearer token. This
token must be different from webhook secrets, node bootstrap tokens, and node
certificates.

```text
Authorization: Bearer <operator-token>
```

Endpoints:

```text
GET /healthz
GET /v1/status
GET /v1/nodes
GET /v1/nodes/{node_id}
GET /v1/services
GET /v1/services/{service_name}
```

`/healthz` is unauthenticated. List responses contain `api_version`,
`observed_at`, `count`, and a deterministically sorted `items` array. Supported
filters are `state` for nodes and `state`, `health`, and `node` for services.

Node capacity is requested capacity, not measured utilization. CPU and memory
`allocated` values are the sum of desired services assigned to the node.
Storage allocation is the durable persistent-volume reservation used by the
scheduler, including retained volumes and the larger of effective/applied size
during a shrink. `available` is capacity minus allocated with a floor of zero;
these values do not represent filesystem I/O or blocks written by guests.
Node storage reports `configured: false` for a pool the node does not have,
so it is never shown as zero capacity; a shared pool configured without an
aggregate capacity (for example, elastic storage) reports `unbounded: true`,
and its capacity and available bytes are not meaningful.

Service list and detail responses build their volume view through one merge,
so their counts and desired, applied, and allocated bytes always agree for
the same snapshot. The merge starts from the durable records, which are kept
when an agent observation is empty or covers only some of the service's
volumes, and overlays the placed node's observation only when it is current.
A zero or omitted observed applied size never replaces a positive durable
one.

The controller folds an agent's volume observation into the durable record
only when it is current evidence: the node is not down, the status is
supported and was received within `node_stale_ttl`, it was made against the
currently published placement and rendered revisions, and it comes from the
node that placement assigns the service to. So neither a stale heartbeat nor
a previous placement of a shared volume can change its resize state.

Missing data fails closed:

- expired leases of `ready` or `draining` nodes become `stale`; a node whose
  current agent reported an explicit shutdown stays `down` after its lease
  ages, because shutdown is a lifecycle fact rather than a transport
  observation;
- unplaced desired services are `pending`, carrying the scheduler's reason code
  (for example `insufficient_compute_capacity`, `volume_capacity_unavailable`,
  `node_storage_exhausted`, `storage_capacity_unknown`, `volume_record_invalid`,
  or `host_port_conflict`, which names the contested port and the service
  already holding it). See [fireworkctl](fireworkctl.md) for what each storage
  reason means and how to resolve it;
- placed services with missing, stale, or unsupported agent status are
  `unknown`;
- VM state and health remain separate, so a service can be `running` and
  `unhealthy`;
- an absent health check is `not_configured` only when fresh agent status is
  available.

The API returns image and kernel basenames, never environment values, kernel
arguments, credentials, full logs, or unbounded errors.

Every endpoint reads one immutable deployment snapshot. It is rebuilt from
object storage at most once per `api_snapshot_ttl` (default 5 s), and
concurrent requests during a rebuild share it, so the overview's parallel
requests and several polling operators cost one scan. `observed_at` is the
snapshot's build time. A failed rebuild is never cached and an older snapshot
is never served in its place. The controller does not use this cache.

A node or volume record that cannot be read or decoded, or whose identity
does not match its key, is left out of the snapshot and listed in `problems`
(`kind`, the identifier from its key, and `record_unreadable` or
`record_invalid`) on list and status responses, instead of failing every
endpoint. Such a fleet is never `converged`: it is `degraded` with
`projection_incomplete`, or `unknown` with `node_record_unreadable` when the
desired revision is empty and the unreadable node could still be running old
workloads. Failing to list records, or to read the desired, placement, or
rendered state, still fails the request.

Lists are not paginated. Each snapshot reads every node and volume record,
which is intended for deployments of up to a few hundred nodes and a few
thousand volumes; responses always contain the complete filtered set.

## Agent convergence status

Registry heartbeats include a bounded `agent_status` schema shared with the
agent-local `/status` endpoint. It reports agent version, observation time,
desired/placement/rendered/applied revisions, reconciliation phase, typed
conditions, and fixed-shape service summaries.

The wire payload accepts at most 16 unique conditions, 256 service summaries,
and 25 volumes per service. Messages are sanitized and capped at 256
characters; revisions, names, condition types, reason codes, agent version,
per-service state/health/network fields, and per-volume identifier/path
fields are all length-bounded too, since 256 services x 25 volumes multiplies
a single unbounded field into megabytes. When an agent has more than 256
desired services it sets
`services_truncated` and reports the full desired count, but that snapshot
cannot prove convergence. Older heartbeats which omit `agent_status` remain
valid and are represented as unknown.

Every request body is capped before it is read or decoded, and a body over
its endpoint's cap receives `413 request_too_large`:

| Endpoint | Cap |
| --- | --- |
| `POST /v1/nodes/enroll` (pre-authentication) | 64 KiB |
| `POST /v1/nodes/renew`, `register`, `{id}/state` | 256 KiB |
| `POST /v1/nodes/heartbeat` | 16 MiB, above the largest valid status |
| `POST /v1/events/github` (before signature verification) | 25 MiB, GitHub's delivery ceiling |
| `POST /login` | 4 KiB |

Status validation is closed-world: `vm_state`, `health`, and volume `type`
and `state` must be values from the published vocabularies, condition types,
service names, and volume IDs within a service must be unique, and sizes must
not be negative. A schema version newer than the control plane supports is
rejected rather than stored. A heartbeat whose envelope is valid but whose
`agent_status` is not still updates liveness, capacity, storage, and host IP;
the stored status is cleared and the response is
`422 invalid_agent_status`, so the agent keeps its registration and the
rejection is visible instead of silent. An agent whose heartbeat is refused
with `413` resends it once without `agent_status` so liveness still lands.

Blocking stages are reported separately: config fetch/parse, capacity, image
sync, host networking, VM reconciliation, and local route publication. Peer
route refresh is non-blocking; failure preserves last-known-good remote routes,
allows the revision to apply, and sets `PeerRoutesReady=false` with a bounded
reason/message.

The local `/status` response, heartbeat payload, and
`firework_agent_status_phase`/`firework_agent_status_condition` gauges are
derived from the same status snapshot. Metric labels contain only the bounded
phase, condition type, and condition status; free-form messages never become
labels.

Agent reconciliation phases are `unknown`, `reconciling`, `ready`, and
`failed`. A service summary is reported as fully current only when the current
desired and placement revisions match and the reporting agent has both observed
and applied the current rendered revision. During publication or rollout
transitions, mismatched placement fails closed to `pending` instead of
presenting an older VM as the current desired service. Stale/down nodes remain
separate lifecycle states and are never inferred to be failed or healthy.

A node that has observed the current rendered revision but not applied it keeps
`reason_code: agent_status_revision_mismatch` on every service placed on it. Its
per-service VM state and health are still projected, because they are fresh and
self-consistent: one service that cannot converge must not erase visibility into
the rest of the node. A service whose own VM state is outside the published
vocabulary — `recovery_pending`, for example — is still reported as `unknown`,
so the service that actually cannot converge remains the one that stands out.
Service detail keeps the stricter rule: runtime fields such as the actual node,
applied revision, PID, and network address appear only for an applied revision.

Older heartbeats without `agent_status` continue to register normally. Their
actual service state is reported as `unknown` until the agent is upgraded.

`GET /v1/status` derives the current fleet revision state from desired,
placement, rendered, and fresh registry snapshots. It does not persist a
second state machine:

- `published`: desired/rendered state exists, but no relevant fresh agent has
  observed the current rendered revision yet;
- `progressing`: at least one relevant agent is applying the current revision,
  or every node converged but a running service's configured health check has
  not passed yet (`service_health_pending`);
- `converged`: every relevant node is fresh and has applied it with no false or
  unknown blocking condition, and every running service is healthy or has no
  health check;
- `degraded`: convergence criteria are met, but at least one node reports a
  non-blocking condition false — the peer-route condition
  (`peer_routes_degraded`), or `VolumeSizesApplied`
  (`volume_size_rejected`), meaning a volume is running at an effective size
  because the requested one was refused. The revision itself also reports
  `volume_size_rejected` while any retained record carries a standing refusal:
  once the refusal is acknowledged the rendered config carries the effective
  size, so the record is the only thing that still knows the operator's request
  stands. A service whose retained volume record could not be read is likewise
  degraded rather than converged: it keeps running its last applied
  configuration, but the desired revision was not applied to it. A fleet
  whose nodes converged but which runs an unhealthy service is degraded with
  `service_unhealthy`;
- `failed`: scheduling left a service pending, or a relevant agent reports a
  blocking failure for the current revision;
- `unknown`: required node status is missing, unsupported, truncated, stale,
  or down.

The response includes deterministic node sets for converged, degraded,
progressing, failed, stale, down, and unknown nodes, plus `services` counts
(`desired`, `running`, `ready`, `pending`, `unhealthy`, `unknown`) taken from
the same per-service projection as `/v1/services` and the sorted
`unhealthy_services` names, so the overview needs no per-node or per-service
request to show what is not serving. `ready` counts running services whose
health check passed or that have none; the agent's own `ready_services` uses
the same rule, so a configured check that has not succeeded yet is never
counted as ready.

Status freshness uses the control plane's receipt clock: a node's status is
current only while both its last heartbeat and the receipt of that status are
within `node_stale_ttl`. The agent's `observed_at` is diagnostic and does not
gate freshness, so agent clock skew cannot hold status open or expire it
early. When a node's status cannot be used, its reason code says why:

| Reason | Meaning |
| --- | --- |
| `node_down` | The current agent reported an explicit shutdown |
| `node_stale` | No heartbeat within `node_stale_ttl` |
| `agent_status_invalid` | The latest heartbeat's status failed validation and was refused with `422` |
| `agent_status_unsupported_schema` | The agent sends a status schema this control plane does not support |
| `agent_status_missing` | The heartbeat carried no status (for example, an older agent) |
| `agent_status_stale` | The stored status is older than `node_stale_ttl` |
| `agent_status_revision_mismatch` | Status is fresh but describes a different revision | A rendered revision with
zero relevant nodes can converge only when the desired service set is also
empty, and even then only once every node the control plane has a record for
confirms it via a fresh heartbeat that has actually observed the empty
revision. The controller currently deletes a no-longer-relevant node's legacy
`nodes/<node>.yaml` instead of publishing an explicit empty config, so an
agent that fetches it sees a missing file, treats that as a fetch failure, and
keeps every VM and route it already had running; treating every known node as
relevant here is what stops an empty desired state from reporting converged
while those workloads are still up.

## CLI

For a first-time walkthrough, see the concise [`fireworkctl` user guide](fireworkctl.md).

Run `fireworkctl` without a command, or with `--help`, to list the available
commands and global options.

```text
fireworkctl nodes
fireworkctl status
fireworkctl node <node-id>
fireworkctl services --health unhealthy
fireworkctl service <service-name> --output json
```

Commands support `--output table|json`; list commands accept the documented
state and health values shown by their subcommand help. All commands accept
`--watch 5s`. Table watch mode refreshes the terminal; JSON watch mode emits
one compact JSON object per line without terminal control sequences. Global
configuration flags must precede the command and support both `--config path`
and `--config=path` forms:

```text
fireworkctl --endpoint https://status.example.com \
  --ca-file /path/to/ca.pem \
  --token-file /path/to/operator-token \
  nodes
```

Configuration precedence is command-line flags, `FIREWORK_API_*` environment
variables, then `~/.config/firework/config.yaml`:

```yaml
endpoint: https://status.example.com
ca_file: /path/to/ca.pem
token_file: /path/to/operator-token
```

Authentication failures exit with code 3 and unknown resources with code 4.

Service detail JSON exposes `observed_at` for the API snapshot and
`service_observed_at` for the latest agent service observation. Port-forward
objects use `host_port` and `vm_port` field names consistently with the rest of
the API. `network_address` reflects the runtime-assigned guest address only
when the reporting agent has applied the current rendered revision.

## Web UI

Open the API origin in a browser and enter the operator token. The server
derives an HTTP-only, secure, same-site session cookie; the token is not stored
in local storage or exposed to JavaScript. The default overview groups node and
service totals by lifecycle state. Each state links to its filtered list, and
the Nodes and Services views link to fixed-order detail tables. Node CPU,
memory, and local-volume storage are shown with allocated/available values and
capacity bars; a pool the node does not have reads "Not configured", never
0 B / 0 B. Node details also show the node's shared pool and backend. Service
lists show a prominent disk summary, and
details show local/shared reservation and applied size plus the per-volume
table, which carries requested, effective, and applied sizes so a refused
request is visible rather than left to a revision diff. Service details also
include a clickable HTTPS public URL when routing
metadata resolves through the API role's `ingress_domain` (or uses an exact
`metadata.host`). The overview's convergence table includes service counts and
links to unhealthy services, and every list and the overview show a notice
naming any record the API skipped. The Services view filters by state,
health, and node, matching the API and CLI filters. All views refresh
automatically; each refresh or navigation cancels the previous request, and a
response that arrives after a newer one started is discarded, so a slow
refresh can never overwrite the view you navigated to.

Rotate access by replacing the operator token secret and restarting the API
role. Existing browser sessions become invalid because their derived session
value no longer matches.
