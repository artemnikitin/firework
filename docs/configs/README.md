# Firework Configuration Reference

This document is the source of truth for Firework configuration formats.

## Configuration Surfaces

There are three main config surfaces:

1. **Agent config** (`/etc/firework/agent.yaml`) - how each node runs.
2. **Enricher input repo** (`defaults.yaml`, `services/*.yaml`, optional `tenants/*`) - user-facing desired state.
3. **Resolved node configs** (`nodes/<node>.yaml`) - fully resolved service assignments consumed by agents.

## 1) Agent Config (`agent.yaml`)

Examples:

- [`examples/agent.yaml`](../../examples/agent.yaml) (Git mode)
- [`examples/agent-s3.yaml`](../../examples/agent-s3.yaml) (S3 mode)
- [`examples/agent-gcs.yaml`](../../examples/agent-gcs.yaml) (GCS mode)
- [`examples/agent-demo.yaml`](../../examples/agent-demo.yaml) (multi-label demo)

### Required rules

- `store_type: "git"` requires `store_url`.
- `store_type: "s3"` requires `s3_bucket`.
- `store_type: "gcs"` requires `gcs_bucket`.
- If `node_name` is empty and `node_names` is empty, hostname is used.
- If `node_names` is empty but `node_name` is set, `node_names` becomes `[node_name]`.

### Field reference

| Field | Required | Default | Notes |
|---|---|---|---|
| `node_id` | no | derived from `node_name` | Stable registry identity for mTLS control-plane integration |
| `node_name` | no | host name | Display/identity name for this node |
| `node_names` | no | derived from `node_name` | Labels to fetch and merge (`nodes/<label>.yaml`) |
| `store_type` | no | `git` | `git`, `s3`, or `gcs` |
| `store_url` | git mode | - | Git repository URL |
| `store_branch` | no | `main` | Git branch |
| `s3_bucket` | s3 mode | - | S3 config bucket |
| `s3_prefix` | no | empty | Prefix before `nodes/` |
| `s3_region` | no | AWS default chain | Region for config bucket |
| `s3_endpoint_url` | no | empty | Custom S3 endpoint (LocalStack/MinIO) |
| `gcs_bucket` | gcs mode | - | GCS config bucket |
| `gcs_prefix` | no | empty | Prefix before `nodes/` |
| `gcs_project` | no | ADC project | GCP project containing the bucket |
| `gcs_credentials_file` | no | ADC | Service-account credentials file; omit on GCE |
| `poll_interval` | no | `30s` | Poll cadence |
| `firecracker_bin` | no | `/usr/bin/firecracker` | Firecracker binary path |
| `state_dir` | no | `/var/lib/firework` | Runtime state (VM sockets/logs) |
| `images_dir` | no | `/var/lib/images` | Local image cache directory |
| `s3_images_bucket` | no | empty | Enables image sync from S3 |
| `gcs_images_bucket` | no | empty | Enables native image sync from GCS |
| `log_level` | no | `info` | `debug`, `info`, `warn`, `error` |
| `api_listen_addr` | no | empty | Enables local API server when set |
| `enable_health_checks` | no | `true` | Health monitor toggle |
| `enable_network_setup` | no | `true` | TAP/bridge/iptables management toggle |
| `vm_subnet` | no | `172.16.0.0/24` | Guest subnet |
| `vm_gateway` | no | `172.16.0.1` | Bridge gateway IP |
| `vm_bridge` | no | `br-firework` | Shared bridge name |
| `out_interface` | no | empty | Outbound NIC for masquerade |
| `enable_capacity_check` | no | `true` | Skip reconcile when desired > node capacity |
| `update_strategy` | no | `all-at-once` | `all-at-once` or `rolling` |
| `update_delay` | no | `0s` | Delay between updates in rolling mode |
| `traefik_config_dir` | no | empty | Enables Traefik dynamic config management for local and remote service routes |
| `ingress_domain` | no | empty | Deployment-owned DNS suffix for `metadata.subdomain`. Final hostname is `<subdomain>.<ingress_domain>`. A bare domain only — no `*.`, scheme, port, or path. Validated/normalized at load (a trailing root dot is stripped) |
| `registry_url` | no | empty | Enables node register/heartbeat to control-plane registry |
| `registry_server_name` | no | empty | Optional TLS server name override for registry endpoint |
| `registry_cert_file` | when `registry_url` set | - | Node mTLS cert path |
| `registry_key_file` | when `registry_url` set | - | Node mTLS key path |
| `registry_ca_file` | when `registry_url` set | - | CA bundle for registry TLS validation (pre-provisioned trust anchor) |
| `registry_bootstrap_token` | no | empty | Bootstrap token used for automated cert enrollment (supports `${ENV_VAR}` expansion) |
| `registry_bootstrap_token_file` | no | empty | File path containing bootstrap token (trimmed; mutually exclusive with `registry_bootstrap_token`) |
| `registry_cert_renew_before` | no | `6h` | Proactive cert renewal window |

Notes for cert lifecycle:

- The bootstrap token is optional for steady-state renewals.
- If mTLS renew is rejected and no bootstrap token is configured, automatic recovery is not possible; rotate/provision node certs manually.

## 2) Enricher Input Repository

Base layout:

```text
defaults.yaml            # optional
services/
  <service>.yaml         # optional, but typically present
tenants/
  <tenant-id>/
    <service>.yaml       # optional tenant expansion/override
```

### 2.1 `defaults.yaml` (optional)

Supported fields:

- `kernel`
- `vcpus`
- `memory_mb`
- `kernel_args`
- `health_check` (`type`, `port`, `path`, `interval`, `timeout`, `retries`)

Fallback precedence for each service field:

`service value` -> `defaults.yaml value` -> `built-in fallback`

Built-in fallbacks:

- `kernel`: `/var/lib/images/vmlinux-5.10`
- `vcpus`: `1`
- `memory_mb`: `256`
- `kernel_args`: `console=ttyS0 reboot=k panic=1 pci=off init=/sbin/fc-init`
- `health_check.interval`: `10s`
- `health_check.timeout`: `5s`
- `health_check.retries`: `3`

### 2.2 `services/*.yaml`

Required fields (validated):

- `name`
- `image`
- `node_type`

Supported fields:

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Service name (unique) |
| `image` | yes | Rootfs path used by runtime |
| `node_type` | yes | Group key used by enricher output |
| `kernel` | no | Kernel path |
| `vcpus` | no | vCPU count |
| `memory_mb` | no | Memory in MiB |
| `kernel_args` | no | Kernel boot args |
| `network` | no | When true, networking is configured |
| `port_forwards` | no | Host-to-guest DNAT mappings; required for remote Traefik routing |
| `health_check` | no | `type` supports `http` or `tcp` |
| `env` | no | Env vars injected via kernel args; values with whitespace are encoded |
| `links` | no | Same-node service links (`env` gets resolved URL) |
| `metadata` | no | Arbitrary key/value tags. Public routing: set **either** `subdomain` (one DNS label; final host is `<subdomain>.<ingress_domain>`) **or** `host` (exact hostname, used verbatim). Setting both is an error |
| `anti_affinity_group` | no | Scheduler anti-affinity preference |
| `cross_node_links` | no | Cross-node env injection (`host_ip:host_port`) |
| `node_host_ip_env` | no | Env var name to inject current node host IP |

### 2.3 `tenants/*` (optional)

Tenant files support two modes:

- **Override mode:** base `services/<name>.yaml` exists -> tenant file overrides non-zero fields.
- **Standalone mode:** no matching base service and `node_type` is set -> tenant file becomes full service spec.

Standalone mode defaults `image` to:

`/var/lib/images/<tenant-id>-<base-file-name>-rootfs.ext4`

Tenant expansions rewrite links to tenant-prefixed service names automatically.

## 3) Control Plane Config (`controlplane.yaml`)

`cmd/controlplane` uses a YAML config file with these important fields:

| Field | Required | Description |
|---|---|---|
| `role` | yes | `registry`, `events`, `controller`, or `all` |
| `registry_listen_addr` | registry/all | HTTPS bind address for registry APIs |
| `events_listen_addr` | events/all | HTTPS bind address for GitHub webhook API |
| `state.backend` | yes | Currently `s3` |
| `state.prefix` | yes | Prefix for control-plane state objects (for example `cp/v1`) |
| `state.s3.bucket` | yes | Bucket for control-plane state and rendered configs |
| `state.s3.region` | no | S3 region |
| `state.s3.endpoint_url` | no | Custom S3 endpoint (MinIO/LocalStack) |
| `leader_lease_ttl` | controller/all | Controller leadership lease TTL |
| `leader_renew_interval` | controller/all | Leadership renewal interval |
| `node_stale_ttl` | controller/all | Freshness threshold for schedulable nodes |
| `controller_tick` | controller/all | Scheduling/publish loop tick |
| `target_branch` | events/all | Git branch filter (default `main`) |
| `config_dir` | no | Optional subdirectory in cloned repo for enrichment input |
| `github_webhook_secret` | events/all | Validates `X-Hub-Signature-256` |
| `tls.cert_file` | role with HTTPS | Server TLS cert |
| `tls.key_file` | role with HTTPS | Server TLS key |
| `tls.client_ca_file` | registry/all | Client cert CA for node mTLS validation |
| `enrollment.ca_file` | registry/all | Node client-cert signing CA cert |
| `enrollment.ca_key_file` | registry/all | Node client-cert signing CA key |
| `enrollment.node_cert_ttl` | no | Issued node cert lifetime |
| `enrollment.bootstrap_tokens` | registry/all | Token list for automated node enrollment |

See [`examples/controlplane.yaml`](../../examples/controlplane.yaml).

## 4) Resolved Node Configs (`nodes/<node>.yaml`)

Agents consume this schema:

```yaml
node: "node-or-instance-id"
host_ip: "10.0.1.42"   # optional, used for cross-node links and remote Traefik routing
services:
  - name: "svc-a"
    image: "/var/lib/images/svc-a-rootfs.ext4"
    kernel: "/var/lib/images/vmlinux-5.10"
    vcpus: 1
    memory_mb: 256
```

Notes:

- `host_ip` is optional and usually added in scheduled multi-node flows. It is used for
  cross-node links and for remote Traefik routes to services on peer nodes.
- Traefik route generation requires `traefik_config_dir` on the agent and a routing key on
  the service — either `metadata.subdomain` or `metadata.host`. Local routes proxy to the
  VM guest IP; remote routes proxy to the peer node's `host_ip` and the first
  `port_forwards[].host_port`.
- `metadata.subdomain` is the portable, deployment-neutral form: it is exactly one DNS
  label and the agent forms the final hostname as `<subdomain>.<ingress_domain>`. It
  requires the agent's `ingress_domain` to be set and `traefik_config_dir` to be enabled;
  otherwise the revision fails rather than silently producing no route. Because deployments
  provision a single-label wildcard certificate (`*.<ingress_domain>`), only one label is
  allowed — `myservice.mysub` is rejected.
- `metadata.host` is an exact hostname used verbatim, retained for backward compatibility
  and custom/internal-host routing. Exact custom hosts require separately compatible DNS and
  TLS configuration that the deployment does not manage. On an installation that leaves
  Traefik management disabled, a legacy `metadata.host` generates no route (historical
  no-op); `metadata.subdomain` is rejected in that mode.
- The same resolver is used for local and remote routes, so a given service resolves to an
  identical `Host(...)` rule regardless of where it is scheduled. Two services that resolve
  to the same hostname fail the sync rather than create nondeterministic equal-priority
  routers.
- Remote Traefik routing is available when the config store can list peer node configs,
  such as the S3-backed control-plane flow.
- `health_check.target` can be set directly, but enriched configs usually use `port`/`path` and let the agent compose the target from guest IP.

## 5) CI-Only Fields (Not Used By Firework Runtime)

Some repositories (for example `firework-gitops-example`) include extra fields for image-build pipelines, such as:

- `source_image`
- `rootfs_size_mb`

These are not part of Firework runtime config structs; they are ignored by Firework and consumed by external CI/build tooling.

## See Also

- High-level overview: [`../../README.md`](../../README.md)
- Architecture: [`../architecture/README.md`](../architecture/README.md)
