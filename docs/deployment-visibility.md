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
GET /v1/nodes
GET /v1/nodes/{node_id}
GET /v1/services
GET /v1/services/{service_name}
```

`/healthz` is unauthenticated. List responses contain `api_version`,
`observed_at`, `count`, and a deterministically sorted `items` array. Supported
filters are `state` for nodes and `state`, `health`, and `node` for services.

Node capacity is requested capacity, not measured utilization. `allocated` is
the sum of desired services assigned to the node, and `available` is total
minus allocated with a floor of zero.

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

Revision states have these meanings:

- `published`: the controller wrote desired, placement, and rendered state;
- `progressing`: a fresh agent reports `reconciling` or has not yet applied the
  rendered revision;
- `converged`: every relevant fresh agent reports the rendered revision as
  applied;
- `degraded`: the revision is applied but one or more services are unhealthy;
- `failed`: a fresh agent reports a bounded reconciliation failure;
- stale/down nodes are separate lifecycle states and are never inferred to be
  failed or healthy.

Older heartbeats without `agent_status` continue to register normally. Their
actual service state is reported as `unknown` until the agent is upgraded.

## CLI

Run `fireworkctl` without a command, or with `--help`, to list the available
commands and global options.

```text
fireworkctl nodes
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
the API.

## Web UI

Open the API origin in a browser and enter the operator token. The server
derives an HTTP-only, secure, same-site session cookie; the token is not stored
in local storage or exposed to JavaScript. The default overview groups node and
service totals by lifecycle state. Each state links to its filtered list, and
the Nodes and Services views link to fixed-order detail tables. All views
refresh automatically.

Rotate access by replacing the operator token secret and restarting the API
role. Existing browser sessions become invalid because their derived session
value no longer matches.
