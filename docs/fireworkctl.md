# `fireworkctl` user guide

`fireworkctl` is the read-only command-line client for the Firework deployment
status API. It shows revision convergence, lists nodes and services, shows
details, and can stream changes.

## Install and configure

From the Firework repository, install the CLI into your Go bin directory:

```bash
make install
fireworkctl --help
```

Create `~/.config/firework/config.yaml` with the status URL and the operator
token file. The deployment guide explains how to fetch that token from AWS or
GCP.

```yaml
endpoint: https://status.example.com
token_file: /absolute/path/to/operator-token
# ca_file: /absolute/path/to/ca.pem  # only for a private/custom CA
```

The endpoint must be the control-plane `api_url` (the `status` origin), not the
`events_webhook_url`. The token file should be readable only by you:

```bash
chmod 0600 /absolute/path/to/operator-token
```

Configuration is resolved in this order: command-line flags, then
`FIREWORK_API_ENDPOINT`, `FIREWORK_API_CA_FILE`, and
`FIREWORK_API_TOKEN_FILE`, then the YAML file. Flags must appear before the
command when they are global options.

## First commands

With no arguments, the CLI prints its command list. The two list commands are
usually the best place to start:

```bash
fireworkctl
fireworkctl status
fireworkctl nodes
fireworkctl services
```

Inspect one item by using the ID or name from a list response:

```bash
fireworkctl node NODE_ID
fireworkctl service SERVICE_NAME
```

List commands support filters and JSON output:

```bash
fireworkctl nodes --state stale
fireworkctl services --state failed
fireworkctl services --health unhealthy --node NODE_ID
fireworkctl services --output json
```

Use `--watch` to poll repeatedly. Table output refreshes the terminal; JSON
watch output emits one JSON object per line:

```bash
fireworkctl nodes --watch 5s
fireworkctl services --output json --watch 5s
```

Every command has focused help:

```bash
fireworkctl nodes --help
fireworkctl services --help
```

## Reading the result

- Revision states from `fireworkctl status`: `published`, `progressing`,
  `converged`, `degraded`, `failed`, `unknown`. The output separates nodes
  which are stale or down from nodes actively reporting a failed apply.
- Node states: `ready`, `draining`, `down`, `stale`, `unknown`.
- Service states: `pending`, `running`, `stopped`, `failed`, `unknown`.
- Service health: `healthy`, `unhealthy`, `unknown`, `not_configured`.

`fireworkctl service SERVICE_NAME` also prints a persistent-volume table when
the service declares volumes. It shows the logical ID, type, guest mount path,
local node binding or shared backend, requested/effective/applied bytes, resize
generation, and preparation state.

Requested is what the repo asked for, effective is what the control plane
accepted and rendered, and applied is what exists on disk. They differ only
when a size request was refused, and in that case the state column names the
refusal reason. See [persistent volumes](persistent-volumes.md).

Storage-related pending and refusal reasons:

- `local_volume_node_unavailable`: retained data is still bound to a node that
  is not currently schedulable. Firework does not replace it with an empty
  volume elsewhere;
- `volume_capacity_unavailable`: the volume cannot bind to any candidate node
  at all — no local pool there, or its binding names somewhere else. A
  placement problem;
- `node_storage_exhausted`: the volume could bind, but no node's pool has room
  for the new reservation. A capacity problem, resolved by freeing retained
  volumes or growing the pool;
- `storage_capacity_unknown`: remaining capacity cannot be verified, because a
  retained record could not be fully read or its bound node is not active.
  New volume-bearing placement waits rather than being allocated against
  capacity that may already be occupied;
- `volume_record_invalid`: the service's own retained record could not be
  parsed. An already-running service keeps running at its last applied
  configuration; one that was never placed stays pending until the record is
  repaired;
- `shrink_below_minimum`: the requested shrink is smaller than the filesystem's
  current contents allow.

Host-port claims have their own reasons — `host_port_conflict` and
`duplicate_host_port_claims` — described in
[docs/configs](configs/README.md#host-port-claims).

`unknown` is intentional: it means the control plane cannot safely confirm the
current state. For example, a stale node or an agent that has not converged to
the current revision is not reported as healthy by inference.

## One-off overrides and common errors

You can override the saved configuration for one command:

```bash
fireworkctl --endpoint https://status.example.com \
  --token-file /absolute/path/to/operator-token nodes
```

Add `--ca-file /absolute/path/to/ca.pem` when the API uses a private CA. A
`401` means the token is missing, wrong, or rotated; a `404` usually means the
node or service name does not exist. The CLI requires an HTTPS endpoint.
