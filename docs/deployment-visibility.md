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
scheduler, including retained volumes and the larger of desired/applied size
during a shrink. `available` is capacity minus allocated with a floor of zero;
these values do not represent filesystem I/O or blocks written by guests.
Service list/detail responses aggregate local and shared volume counts plus
desired, applied, and allocated bytes. Service detail also retains the durable
per-volume view when an agent observation is empty or only contains a subset
of the service's volumes.

Missing data fails closed:

- expired node leases become `stale`;
- unplaced desired services are `pending`;
- placed services with missing, stale, or unsupported agent status are
  `unknown`;
- VM state and health remain separate, so a service can be `running` and
  `unhealthy`;
- an absent health check is `not_configured` only when fresh agent status is
  available.

The API returns image and kernel basenames, never environment values, kernel
arguments, credentials, full logs, or unbounded errors.

## Agent convergence status

Registry heartbeats include a bounded `agent_status` schema shared with the
agent-local `/status` endpoint. It reports agent version, observation time,
desired/placement/rendered/applied revisions, reconciliation phase, typed
conditions, and fixed-shape service summaries.

The wire payload accepts at most 16 unique conditions, 256 service summaries,
and 25 volumes per service. Messages are sanitized and capped at 256
characters; revisions, names, condition types, and reason codes are also
length-bounded. When an agent has more than 256 desired services it sets
`services_truncated` and reports the full desired count, but that snapshot
cannot prove convergence. Registry requests are body-size limited and reject
duplicate or invalid bounded fields. Older heartbeats which omit
`agent_status` remain valid and are represented as unknown.

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
`failed`. Desired service VM/health state is trusted only when the current
desired and placement revisions match and the reporting agent has both observed
and applied the current rendered revision. During publication or rollout
transitions, mismatched data fails closed to `pending` or `unknown` instead of
presenting an older VM as the current desired service. Stale/down nodes remain
separate lifecycle states and are never inferred to be failed or healthy.

Older heartbeats without `agent_status` continue to register normally. Their
actual service state is reported as `unknown` until the agent is upgraded.

`GET /v1/status` derives the current fleet revision state from desired,
placement, rendered, and fresh registry snapshots. It does not persist a
second state machine:

- `published`: desired/rendered state exists, but no relevant fresh agent has
  observed the current rendered revision yet;
- `progressing`: at least one relevant agent is applying the current revision;
- `converged`: every relevant node is fresh and has applied it with no false or
  unknown blocking condition;
- `degraded`: convergence criteria are met, but at least one node reports the
  non-blocking peer-route condition false;
- `failed`: scheduling left a service pending, or a relevant agent reports a
  blocking failure for the current revision;
- `unknown`: required node status is missing, unsupported, truncated, stale,
  or down.

The response includes deterministic node sets for converged, degraded,
progressing, failed, stale, down, and unknown nodes. A rendered revision with
zero relevant nodes converges only when the desired service set is also empty.

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
capacity bars. Shared storage is a backend-level resource, so it is not
duplicated on every node. Service lists show a prominent disk summary, and
details show local/shared reservation and applied size plus the per-volume
table. Service details also include a clickable HTTPS public URL when routing
metadata resolves through the API role's `ingress_domain` (or uses an exact
`metadata.host`). All views refresh automatically.

Rotate access by replacing the operator token secret and restarting the API
role. Existing browser sessions become invalid because their derived session
value no longer matches.
