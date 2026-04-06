# Development Guide

## Prerequisites

| Tool | Required for |
|------|-------------|
| Go 1.25+ | building and testing everything |
| `git` | smoke test, go-git dependency |
| `curl` | smoke test |
| `make` | build targets |
| `staticcheck` | linting (optional — `go install honnef.co/go/tools/cmd/staticcheck@latest`) |
| Linux host with `/dev/kvm` | running actual Firecracker VMs |

Most development (coding, unit tests, smoke test) works on macOS or Linux without KVM. You only need a real metal node to test actual VM lifecycle.

## Repository Layout

```
cmd/
  agent/       firework-agent entry point
  controlplane/ firework-controlplane entry point (roles: registry/events/controller/all)
  fc-init/     guest init process (runs as PID 1 inside each VM)
internal/
  agent/       reconciliation loop, metrics publishing
  controlplane/ control-plane state, APIs, leader election, scheduling runtime
  config/      config types and YAML loader
  enricher/    enrichment pipeline (spec → node config)
  scheduler/   bin-packing placement algorithm
  reconciler/  plan/apply VM change logic
  vm/          Firecracker process lifecycle
  network/     bridge/TAP/iptables setup
  healthcheck/ HTTP and TCP health monitors
  traefik/     dynamic config file management
  store/       Git and S3 config store backends
  imagesync/   S3 image sync
  capacity/    node vCPU/memory discovery
  api/         HTTP status/health/metrics server
examples/      example agent and control-plane configs
scripts/       smoke-local.sh
docs/          architecture and config reference
```

## Building

```bash
make help          # show all targets with descriptions
```

| Target | Output | When to use |
|--------|--------|-------------|
| `make build-agent` | `bin/firework-agent` (native OS) | local testing, smoke test |
| `make build-controlplane` | `bin/firework-controlplane` (native OS) | local control-plane testing |
| `make build-linux-amd64` | `bin/firework-agent-linux-amd64` + `bin/firework-controlplane-linux-amd64` + `bin/fc-init-linux-amd64` | producing Linux x86_64 artifacts |
| `make build-linux-arm64` | `bin/firework-agent-linux-arm64` + `bin/firework-controlplane-linux-arm64` + `bin/fc-init-linux-arm64` (+ `bin/fc-init` alias) | updating ARM64 nodes or Packer AMI builds |
| `make build-fc-init` | `bin/fc-init-linux-arm64` + `bin/fc-init` alias (Linux ARM64, static) | updating the guest init binary in a rootfs image |
| `make build-all` | native binaries + Linux amd64/arm64 variants for all binaries | full release build |
| `make clean` | removes `bin/` and test cache | — |

`fc-init` cross-compiled artifacts are static (`CGO_ENABLED=0`). The compatibility
alias `bin/fc-init` points to the Linux/ARM64 build because the guest runtime
still uses ARM64 by default.

Version, commit, and build time are injected at link time via `ldflags` and reported by `firework-agent --version`.

## Testing

### Unit tests

```bash
make test          # all packages, no caching
make test-race     # with race detector
make test-cover    # with coverage report (outputs coverage.out)
```

All unit tests run without AWS credentials, KVM, or network access. AWS interactions are mocked; the scheduler and reconciler are pure functions with no external dependencies.

### Smoke test

```bash
make smoke-local
```

Runs a real `firework-agent` binary (built on the fly) against a local Git repo with a fake Firecracker binary. No KVM or AWS required. Covers the full reconcile loop: startup, config detection, VM launch, config update, reconciliation, and `/status` API.

Useful environment variables:

```bash
SMOKE_API_PORT=18082 make smoke-local   # change API port if 18081 is in use
KEEP_SMOKE_TMP=1 make smoke-local       # keep the temp workspace for inspection
POLL_INTERVAL=1s make smoke-local       # speed up the poll cycle
```

The smoke workspace (agent config, fake Firecracker log, agent log) is printed on failure so you can inspect what went wrong.

### Linting

```bash
make vet           # go vet only
make lint          # go vet + staticcheck (if installed)
make fmt           # gofmt -s in place
make tidy          # go mod tidy + verify
```

## Running the Control Plane

Build and run the new control-plane binary:

```bash
make build-controlplane
bin/firework-controlplane --config examples/controlplane.yaml
```

Role modes:

- `registry`: node enrollment/register/heartbeat APIs
- `events`: GitHub webhook ingestion
- `controller`: leader-elected scheduler/publisher loop
- `all`: all roles in one process

## Control-Plane Container (GHCR)

Release tags publish a multi-arch image to:

- `ghcr.io/artemnikitin/firework-controlplane:<tag>`

### Local push for pre-merge deployment testing

Useful when testing changes with `firework-deployment-example` before merge.

1. Log in to GHCR:

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GHCR_USER" --password-stdin
```

`GHCR_TOKEN` should have `write:packages` (and usually `read:packages`).

2. Build and push a test tag:

```bash
scripts/push-controlplane-image.sh ghcr.io/<owner>/firework-controlplane:<test-tag>
```

The helper auto-creates a `docker-container` buildx builder for multi-platform
pushes when the current driver does not support it.

If you only need a single-arch image, override platforms:

```bash
PLATFORMS=linux/amd64 scripts/push-controlplane-image.sh ghcr.io/<owner>/firework-controlplane:<test-tag>
```

Example:

```bash
scripts/push-controlplane-image.sh ghcr.io/artemnikitin/firework-controlplane:dev-$(date +%Y%m%d%H%M)
```

Alternative via Makefile:

```bash
make docker-push-controlplane-image \
  CONTROLPLANE_IMAGE=ghcr.io/<owner>/firework-controlplane \
  IMAGE_TAG=<test-tag>
```

The deployment repo can then reference that tag (or digest) directly.

## End-to-End Deployment Flow

See [firework-deployment-example](https://github.com/artemnikitin/firework-deployment-example) for Terraform and Packer sources. The high-level order:

1. **Build AMI** — run Packer with the agent and Traefik binaries baked in.
2. **Deploy control plane** — Terraform creates control-plane instances and S3 state bucket.
3. **Deploy data plane** — Terraform creates the VPC, ALB, and Auto Scaling Group using the AMI from step 1.
4. **Configure GitHub webhook** — point the config repo's webhook at the control-plane `events` role endpoint with the shared secret.
5. **Push a config** — GitHub webhook triggers the `events` role, controller schedules and publishes rendered `nodes/*.yaml` to S3. Agents pick it up on their next poll.

For IAM setup, refer to `iam-policies/` in the deployment repo. The policy for the deployment user has a ~6 KB size limit, so check coverage before adding new AWS resources to Terraform.

## Code Conventions

- Format with `gofmt -s` (`make fmt`).
- Tests are table-driven where multiple cases exist.
- AWS-dependent code is behind interfaces so unit tests can substitute fakes (see `store.Store`, `vm.Manager`).
- The scheduler and enricher pipelines are pure functions — prefer keeping them that way.
