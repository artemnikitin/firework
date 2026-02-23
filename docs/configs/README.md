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
- [`examples/agent-demo.yaml`](../../examples/agent-demo.yaml) (multi-label demo)

### Required rules

- `store_type: "git"` requires `store_url`.
- `store_type: "s3"` requires `s3_bucket`.
- If `node_name` is empty and `node_names` is empty, hostname is used.
- If `node_names` is empty but `node_name` is set, `node_names` becomes `[node_name]`.

### Field reference

| Field | Required | Default | Notes |
|---|---|---|---|
| `node_name` | no | host name | Display/identity name for this node |
| `node_names` | no | derived from `node_name` | Labels to fetch and merge (`nodes/<label>.yaml`) |
| `store_type` | no | `git` | `git` or `s3` |
| `store_url` | git mode | - | Git repository URL |
| `store_branch` | no | `main` | Git branch |
| `s3_bucket` | s3 mode | - | S3 config bucket |
| `s3_prefix` | no | empty | Prefix before `nodes/` |
| `s3_region` | no | AWS default chain | Region for config bucket |
| `s3_endpoint_url` | no | empty | Custom S3 endpoint (LocalStack/MinIO) |
| `poll_interval` | no | `30s` | Poll cadence |
| `firecracker_bin` | no | `/usr/bin/firecracker` | Firecracker binary path |
| `state_dir` | no | `/var/lib/firework` | Runtime state (VM sockets/logs) |
| `images_dir` | no | `/var/lib/images` | Local image cache directory |
| `s3_images_bucket` | no | empty | Enables image sync from S3 |
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
| `traefik_config_dir` | no | empty | Enables Traefik dynamic config management |

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
| `port_forwards` | no | Host-to-guest DNAT mappings |
| `health_check` | no | `type` supports `http` or `tcp` |
| `env` | no | Env vars injected via kernel args |
| `links` | no | Same-node service links (`env` gets resolved URL) |
| `metadata` | no | Arbitrary key/value tags |
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

## 3) Enricher Runtime Environment (Lambda)

`cmd/enricher` supports these env vars:

| Variable | Required | Description |
|---|---|---|
| `S3_BUCKET` | yes | Destination bucket for `nodes/*.yaml` |
| `S3_PREFIX` | no | Prefix before `nodes/` |
| `S3_REGION` | no | Region for S3 client |
| `S3_ENDPOINT_URL` | no | Custom S3 endpoint |
| `TARGET_BRANCH` | no | Branch filter for webhook events (default `main`) |
| `CONFIG_DIR` | no | Subdirectory in cloned repo containing config files |
| `GITHUB_WEBHOOK_SECRET` | no | Validate `X-Hub-Signature-256` |
| `GITHUB_TOKEN` | no | Clone private GitHub repos |
| `CONFIG_REPO_URL` | scheduled mode only | Repo URL for EventBridge-triggered runs |
| `SCHEDULER_LAMBDA_ARN` | no | Enables scheduler invocation |
| `SCHEDULER_REGION` | no | Scheduler invoke region (defaults to `S3_REGION`) |
| `EC2_REGION` | no | Region for resolving node private IPs |

## 4) Scheduler Runtime Environment (Lambda)

`cmd/scheduler` supports:

| Variable | Required | Description |
|---|---|---|
| `CW_NAMESPACE` | yes | Namespace containing `firework_node_*` metrics |
| `S3_BUCKET` | no | Reads existing placement from this bucket |
| `S3_PREFIX` | no | Prefix before `nodes/` |
| `S3_REGION` | no | Region for CloudWatch + S3 clients |

## 5) Resolved Node Configs (`nodes/<node>.yaml`)

Agents consume this schema:

```yaml
node: "node-or-instance-id"
host_ip: "10.0.1.42"   # optional, used for cross-node links
services:
  - name: "svc-a"
    image: "/var/lib/images/svc-a-rootfs.ext4"
    kernel: "/var/lib/images/vmlinux-5.10"
    vcpus: 1
    memory_mb: 256
```

Notes:

- `host_ip` is optional and usually added in scheduled multi-node flows.
- `health_check.target` can be set directly, but enriched configs usually use `port`/`path` and let the agent compose the target from guest IP.

## 6) CI-Only Fields (Not Used By Firework Runtime)

Some repositories (for example `firework-gitops-example`) include extra fields for image-build pipelines, such as:

- `source_image`
- `rootfs_size_mb`

These are not part of Firework runtime config structs; they are ignored by Firework and consumed by external CI/build tooling.

## See Also

- High-level overview: [`../../README.md`](../../README.md)
- Architecture: [`../architecture/README.md`](../architecture/README.md)
