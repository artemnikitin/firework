# <img src="logo.svg" alt="firework logo" height="48"/> 

A lightweight, pull-based orchestrator for services running in [Firecracker](https://firecracker-microvm.github.io/) micro VMs.

## Overview

Firework consists of two binaries:

- **Agent** (`firework-agent`) — runs on each node, periodically pulling desired state from a config store (S3 or Git) and reconciling it with running Firecracker microVMs.
- **Enricher** (`enricher`) — an AWS Lambda function that transforms minimal, user-friendly service definitions from a Git repo into fully resolved per-node configs and writes them to S3.

The design separates infrastructure management (handled by Terraform or similar) from application management (handled by Firework).

### Architecture

The recommended production flow uses S3 as the config store, with the enricher Lambda in between:

```
User Config (Git)       Enricher Lambda          S3 Bucket            Node Agent
┌───────────┐          ┌────────────────┐       ┌──────────┐   pull  ┌──────────┐
│ defaults  │  push    │ Load input     │  put  │ nodes/   │◄───────│  Agent   │
│ .yaml     │─────────►│ Validate       │──────►│  gp.yaml │ (poll) │          │
│ services/ │ webhook  │ Enrich defaults│       │  cp.yaml │        │ reconcile│
│  web.yaml │          │ Group by type  │       └──────────┘        └────┬─────┘
│  api.yaml │          │ Write to S3    │                                │
└───────────┘          └────────────────┘                       ┌───────┴───────┐
                                                                │ Firecracker   │
                                                                │  VM: web      │
                                                                │  VM: api      │
                                                                └───────────────┘
```

The agent also supports pulling directly from a Git repo (simpler setup, no enrichment needed — you write fully resolved node configs by hand). Git operations use the pure-Go [go-git](https://github.com/go-git/go-git) library — no external `git` binary is required on the host or in the Lambda environment.

### How it works

**Agent (runs on each node):**

1. **Pull** — polls the config store (S3 or Git) on a configurable interval (default: 30s)
2. **Diff** — desired state (from YAML config) is compared with actual state (running VMs)
3. **Converge** — missing services are created, extra services are removed, changed services are recreated
4. **Link** — service dependencies are resolved: the agent looks up each linked service's guest IP and injects connection URLs as environment variables
5. **Monitor** — health checks run on configured services; unhealthy services are automatically restarted

**Enricher (runs as Lambda on push):**

1. **Load** — clones the Git repo (using go-git with optional GitHub token auth) and reads `defaults.yaml` + `services/*.yaml`
2. **Validate** — checks for missing required fields, duplicate names, invalid health check types
3. **Enrich** — fills in missing fields from defaults, then from hardcoded fallbacks (priority: service spec > defaults.yaml > fallback)
4. **Group** — groups services by `node_type` to produce one config file per node type
5. **Write** — uploads enriched `nodes/<node-type>.yaml` files to S3

## Project Structure

```
cmd/
  agent/                — Agent binary entry point
  enricher/             — Enricher Lambda entry point
internal/
  agent/                — Core agent loop (poll → diff → converge → link → monitor)
  api/                  — HTTP status/health API server
  config/               — Configuration types and YAML parsing
  enricher/             — Enrichment pipeline (input, defaults, validation, S3 writer)
  healthcheck/          — Health check monitor (HTTP, TCP) with auto-restart
  imagesync/            — S3 image sync with ETag-based caching
  network/              — TAP device, shared bridge, and port forwarding
  reconciler/           — Desired vs actual state diffing and convergence
  store/                — Config store interface (S3 + Git implementations)
  version/              — Build version info
  vm/                   — Firecracker VM lifecycle management
examples/
  agent.yaml            — Example agent config (Git store)
  agent-s3.yaml         — Example agent config (S3 store)
  enricher-input/       — Example enricher input (user-facing config)
    defaults.yaml       — Global defaults applied to every service
    services/
      web-api.yaml      — Service definition (minimal, enriched by Lambda)
      worker.yaml       — Service definition
  nodes/
    node-1.yaml         — Example enriched node config (agent input)
```

## Quick Start

### Prerequisites

- Go 1.25+
- [Firecracker](https://github.com/firecracker-microvm/firecracker/releases) binary
- A Linux host with KVM support (`/dev/kvm`)
- A config store: S3 bucket (recommended) or Git repo

### Build

```bash
# Build the agent
make build

# Build the enricher Lambda (cross-compiled for linux/arm64)
make build-enricher

# Package the enricher as a Lambda-ready ZIP
make package-enricher
```

### Configure

Firework supports two deployment modes:

#### Mode A: S3 store + Enricher Lambda (recommended)

Users write minimal service specs in a Git repo. On push, the enricher Lambda validates, enriches, and writes fully resolved configs to S3. The agent on each node polls S3.

**1. Set up the enricher input repo:**

See [firework-gitops-example](https://github.com/artemnikitin/firework-gitops-example) for a complete, working example — including a CI pipeline that builds ext4 rootfs images from Docker images, config overlays, and service links.

```
your-config-repo/
  defaults.yaml           # optional global defaults
  services/
    web-api.yaml
    worker.yaml
  configs/                # optional per-service config overlays
    web-api/
      etc/app/config.yaml
```

`defaults.yaml` — applied to every service unless overridden:

```yaml
kernel: "/var/lib/images/vmlinux-5.10"
vcpus: 1
memory_mb: 256
kernel_args: "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/fc-init"
health_check:
  type: "http"
  port: 8080
  path: "/health"
  interval: "10s"
  timeout: "5s"
  retries: 3
```

`services/web-api.yaml` — one file per service:

```yaml
name: "web-api"
source_image: "nginx:alpine"
image: "/var/lib/images/web-api-rootfs.ext4"
node_type: "general-purpose"
vcpus: 2
memory_mb: 512
network: true
port_forwards:
  - host_port: 80
    vm_port: 8080
health_check:
  type: "http"
  port: 8080
  path: "/health"
metadata:
  version: "1.2.3"
```

The `source_image` field specifies the Docker image to convert into an ext4 rootfs in CI. The enricher ignores unknown fields, so this is backwards-compatible.

The `node_type` field determines which node config file the service ends up in. All services with `node_type: "general-purpose"` are grouped into `nodes/general-purpose.yaml` in S3. The agent's `node_name` should match the node type it should run.

**2. Deploy the enricher Lambda:**

Build and deploy the enricher:

```bash
make package-enricher
# Deploy bin/enricher.zip as an AWS Lambda function (arm64 runtime)
```

Required Lambda environment variables:

| Variable | Required | Description |
|---|---|---|
| `S3_BUCKET` | yes | Target S3 bucket for enriched configs |
| `S3_PREFIX` | no | Key prefix (e.g. `prod/`), include trailing slash |
| `S3_REGION` | no | AWS region, resolved from environment if empty |
| `S3_ENDPOINT_URL` | no | Custom endpoint for LocalStack/MinIO |
| `TARGET_BRANCH` | no | Git branch to process (default: `main`) |
| `GITHUB_TOKEN` | no | Personal access token for cloning private repos |

The Lambda is triggered by a GitHub push webhook (via API Gateway). It receives the webhook payload, clones the repo using go-git with optional token authentication, runs the enrichment pipeline, and writes results to S3.

Required IAM permissions for the enricher Lambda:

```json
{
  "Effect": "Allow",
  "Action": ["s3:PutObject", "s3:GetObject"],
  "Resource": "arn:aws:s3:::my-firework-configs/nodes/*"
}
```

**3. Configure the agent:**

```yaml
node_name: "general-purpose"
store_type: "s3"
s3_bucket: "my-firework-configs"
s3_region: "us-east-1"
poll_interval: "30s"
firecracker_bin: "/usr/bin/firecracker"
state_dir: "/var/lib/firework"
images_dir: "/var/lib/images"
log_level: "info"
api_listen_addr: ":8080"
vm_subnet: "172.16.0.0/24"
vm_gateway: "172.16.0.1"
vm_bridge: "br-firework"
```

The agent's `node_name` must match the `node_type` used in the service specs so it pulls the correct config file from S3. For multi-label setups, use `node_names` instead (see [Multi-Label Support](#multi-label-support)).

Required IAM permissions for the agent:

```json
{
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:HeadObject"],
  "Resource": "arn:aws:s3:::my-firework-configs/nodes/*"
}
```

#### Mode B: Git store (simpler, no enrichment)

Users write fully resolved node configs directly in a Git repo. The agent clones the repo using go-git and reads its config directly. No enricher needed. No external `git` binary required.

**1. Set up the config repo:**

```
your-config-repo/
  nodes/
    node-1.yaml
    node-2.yaml
```

`nodes/node-1.yaml` — fully specified:

```yaml
node: "node-1"
services:
  - name: "web-api"
    image: "/var/lib/images/web-api-rootfs.ext4"
    kernel: "/var/lib/images/vmlinux-5.10"
    vcpus: 2
    memory_mb: 512
    kernel_args: "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/fc-init ip=172.16.0.2::172.16.0.1:255.255.255.0::eth0:off"
    network:
      interface: "tap-web-api"
      guest_mac: "AA:FC:00:00:00:01"
      guest_ip: "172.16.0.2"
    port_forwards:
      - host_port: 80
        vm_port: 8080
    health_check:
      type: "http"
      port: 8080
      path: "/health"
      interval: "10s"
      timeout: "5s"
      retries: 3
```

**2. Configure the agent:**

```yaml
node_name: "node-1"
store_type: "git"
store_url: "https://github.com/artemnikitin/firework-gitops-example.git"
store_branch: "main"
poll_interval: "30s"
firecracker_bin: "/usr/bin/firecracker"
state_dir: "/var/lib/firework"
log_level: "info"
api_listen_addr: ":8080"
```

### Run

```bash
sudo ./bin/firework-agent --config /etc/firework/agent.yaml
```

### Other commands

```bash
make build             # build the agent
make build-enricher    # build the enricher Lambda
make package-enricher  # package enricher as Lambda ZIP
make test              # run tests
make test-race         # run tests with race detector
make test-cover        # run tests with coverage report
make lint              # run linters
make fmt               # format code
make help              # show all targets
```

### Release

Releases are automated with [GoReleaser](https://goreleaser.com/) via GitHub Actions on tag push (`v*`).

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow publishes these assets to GitHub Releases:

- `firework-agent-linux-amd64`
- `firework-agent-linux-arm64`
- `firework-agent-darwin-arm64`
- `fc-init-linux-arm64`
- `firework-enricher-bootstrap-linux-arm64`
- `firework-enricher-lambda-linux-arm64.zip`
- `enricher.zip` (legacy compatibility asset)
- `SHA256SUMS`

To test release packaging locally without publishing:

```bash
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

## Agent Configuration Reference

| Field | Required | Default | Description |
|---|---|---|---|
| `node_name` | no | hostname | Unique node identifier; determines which config file to pull |
| `node_names` | no | — | Multi-label: list of node types to fetch and merge. Overrides `node_name` |
| `store_type` | no | `git` | Config store backend: `git` or `s3` |
| `store_url` | git only | — | Git repo URL |
| `store_branch` | no | `main` | Git branch to track |
| `s3_bucket` | s3 only | — | S3 bucket name |
| `s3_prefix` | no | — | S3 key prefix (e.g. `prod/`), include trailing slash |
| `s3_region` | no | from env | AWS region |
| `s3_endpoint_url` | no | — | Custom S3 endpoint for LocalStack/MinIO |
| `s3_images_bucket` | no | — | S3 bucket for VM images; enables automatic image sync |
| `images_dir` | no | `/var/lib/images` | Local directory for VM images |
| `poll_interval` | no | `30s` | How often to poll the config store |
| `firecracker_bin` | no | `/usr/bin/firecracker` | Path to the Firecracker binary |
| `state_dir` | no | `/var/lib/firework` | Directory for runtime state (VM sockets, logs) |
| `log_level` | no | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `api_listen_addr` | no | — | Address for the HTTP API (e.g. `:8080`); empty disables it |
| `enable_health_checks` | no | `true` | Enable health check monitoring |
| `enable_network_setup` | no | `true` | Enable automatic TAP/bridge network setup |
| `vm_subnet` | no | `172.16.0.0/24` | CIDR subnet for VM guest IPs |
| `vm_gateway` | no | `172.16.0.1` | Gateway IP assigned to the shared bridge |
| `vm_bridge` | no | `br-firework` | Shared bridge device name |
| `out_interface` | no | — | Host NIC for iptables masquerade (internet access for VMs) |

## Enricher

The enricher is an AWS Lambda function that bridges user-friendly service definitions to fully resolved agent configs.

### Input format

The enricher reads from a Git repo with this layout:

```
defaults.yaml           # optional
services/
  <service-name>.yaml   # one per service
```

**Service spec fields:**

| Field | Required | Description |
|---|---|---|
| `name` | yes | Unique service name |
| `image` | yes | Path to the root filesystem image |
| `node_type` | yes | Determines which node config the service is grouped into |
| `kernel` | no | Kernel binary path (falls back to defaults, then `/var/lib/images/vmlinux-5.10`) |
| `vcpus` | no | Virtual CPUs (falls back to defaults, then `1`) |
| `memory_mb` | no | Memory in MB (falls back to defaults, then `256`) |
| `kernel_args` | no | Kernel boot args (falls back to defaults, then `console=ttyS0 reboot=k panic=1 pci=off init=/sbin/fc-init`) |
| `network` | no | `true`/`false` — whether the service needs networking |
| `port_forwards` | no | List of `{host_port, vm_port}` mappings for host-to-VM port forwarding |
| `env` | no | Key-value map of environment variables injected at runtime via kernel boot args |
| `links` | no | List of service links for automatic inter-service connectivity (see [Service Links](#service-links)) |
| `health_check` | no | Health check config (`type`, `port`, `path`, `interval`, `timeout`, `retries`) |
| `metadata` | no | Arbitrary key-value pairs passed through to the agent |

### Enrichment rules

Field values are resolved with this priority: **service spec > defaults.yaml > hardcoded fallback**.

Health checks in the enricher use `port` + `path` instead of a full `target` URL. The agent composes the full target from the guest IP at runtime.

### Output

The enricher groups services by `node_type` and writes one YAML file per group to S3. Any stale `nodes/*.yaml` objects for node types that are no longer present are removed during each run:

```
s3://my-firework-configs/
  nodes/general-purpose.yaml    # all services with node_type: "general-purpose"
  nodes/compute.yaml            # all services with node_type: "compute"
```

## Service Links

Services can declare dependencies on other services running on the same node. The agent automatically resolves these at runtime — users never need to know or manage guest IPs.

### How it works

1. A service declares a link in its definition:

```yaml
name: "kibana"
links:
  - service: "elasticsearch"
    env: "ELASTICSEARCH_HOSTS"
    port: 9200
```

2. During reconciliation, after the agent assigns guest IPs to all services, it resolves each link:
   - Looks up the target service's assigned guest IP
   - Constructs a URL: `http://<target-ip>:<port>` (protocol defaults to `http`, configurable via `protocol` field)
   - Injects it into the dependent service's environment as the specified variable

3. The environment variable is passed to the guest VM via kernel boot arguments (`firework.env.ELASTICSEARCH_HOSTS=http://172.16.0.2:9200`). The guest's `fc-init` parses `/proc/cmdline` and exports it before starting the application.

### Link fields

| Field | Required | Description |
|---|---|---|
| `service` | yes | Name of the target service (must be on the same node) |
| `env` | yes | Environment variable name to inject (e.g. `ELASTICSEARCH_HOSTS`) |
| `port` | yes | Target service's port |
| `protocol` | no | URL scheme (default: `http`) |

This replaces the need for hardcoded IPs in application configs. For example, Kibana's `kibana.yml` can use `elasticsearch.hosts: ["${ELASTICSEARCH_HOSTS}"]` and the agent fills in the correct URL at boot time.

## HTTP API

When `api_listen_addr` is configured, the agent exposes three endpoints:

### `GET /healthz` — Agent liveness

```json
{ "status": "ok", "time": "2026-02-05T12:00:00Z" }
```

### `GET /status` — Full agent status

```json
{
  "node": "node-1",
  "last_revision": "\"etag-abc123\"",
  "services": [
    {
      "name": "web-api",
      "state": "running",
      "pid": 12345,
      "health": {
        "status": "healthy",
        "last_checked": "2026-02-05T12:00:00Z",
        "failures": 0,
        "last_error": ""
      }
    }
  ]
}
```

### `GET /health` — Health check results only

```json
{
  "checks": {
    "web-api": { "status": "healthy", "failures": 0, "last_error": "" }
  }
}
```

## Health Checks

Services can define health checks that run on a configurable interval:

- **HTTP** — `GET` request, expects 2xx response
- **TCP** — attempts to open a TCP connection

When a service exceeds the failure threshold (`retries`), it is automatically restarted (stop + start).

## Network Setup

When `enable_network_setup` is true, the agent automatically:

1. Creates a shared bridge (`vm_bridge`, default `br-firework`) with the gateway IP
2. Creates a TAP device for each service with a `network` config and attaches it to the shared bridge
3. Assigns sequential guest IPs from `vm_subnet` (starting at `.2`) and MACs to each service (sorted alphabetically by name for deterministic allocation)
4. Appends Linux kernel IP autoconfig (`ip=<guest>::<gw>:<mask>::eth0:off`) to kernel args so the guest configures networking before init runs
5. Configures iptables masquerading for internet access (if `out_interface` is set)
6. Sets up iptables DNAT rules for `port_forwards` (host port → guest IP:VM port)
7. Resolves service links to concrete URLs using assigned guest IPs
8. Injects environment variables into kernel boot args (`firework.env.KEY=VALUE`)
9. Tears down port forwards, TAP devices, and network on service removal

All networking is managed by the agent — no manual bridge creation, iptables rules, or IP management is needed on the host.

This requires running the agent as root (or with `CAP_NET_ADMIN`).

## Image Sync

When `s3_images_bucket` is set, the agent downloads VM images (rootfs and kernel files) from S3 to the local `images_dir` before starting VMs. It uses S3 ETags with local sidecar files to skip re-downloading unchanged images. If an image is not found in S3 but exists locally (e.g. a kernel baked into the AMI by Packer), it is silently skipped.

## Multi-Label Support

A single node can serve multiple roles by setting `node_names` in the agent config:

```yaml
node_names:
  - "web"
  - "backend"
```

The agent fetches configs for each label (e.g. `nodes/web.yaml` and `nodes/backend.yaml` from S3) and merges all services. If two configs define a service with the same name, the last one wins (with a warning). Services are sorted by name for deterministic IP allocation.

## Environment Variables

Service definitions can include an `env` map for runtime configuration:

```yaml
env:
  SERVER_HOST: "0.0.0.0"
  LOG_LEVEL: "debug"
```

The agent injects these as `firework.env.KEY=VALUE` entries in the kernel command line. The guest's `/sbin/fc-init` parses `/proc/cmdline` and exports them before launching the application. This is useful for per-deployment settings without rebuilding images.

Environment variables from `links` are merged into `env` automatically — link-injected values take precedence.

## Testing

```bash
make test          # run all tests
make test-verbose  # with verbose output
make test-race     # with race detector
make test-cover    # with coverage report
```

## Related Repositories

- [firework-deployment-example](https://github.com/artemnikitin/firework-deployment-example) — Terraform + Packer setup for deploying Firework on AWS (VPC, EC2, ALB, Lambda enricher, S3 buckets, AMI build)
- [firework-gitops-example](https://github.com/artemnikitin/firework-gitops-example) — Example GitOps config repo with Kibana + Elasticsearch service definitions, config overlays, and a CI pipeline for building ext4 rootfs images

## Roadmap

- [x] Config enrichment layer (Lambda)
- [x] Service-to-service discovery (service links)
- [ ] Webhook/watch mode for faster config propagation
- [ ] Metrics export (Prometheus)
- [ ] Secret management (encrypted values in config)
- [ ] Image pulling from OCI registries

## License

MIT
