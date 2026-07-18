# `fireworkctl` user guide

`fireworkctl` is the read-only command-line client for the Firework deployment
status API. It lists nodes and services, shows details, and can stream changes.

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

- Node states: `ready`, `draining`, `down`, `stale`, `unknown`.
- Service states: `pending`, `running`, `stopped`, `failed`, `unknown`.
- Service health: `healthy`, `unhealthy`, `unknown`, `not_configured`.

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
