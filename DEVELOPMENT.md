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
  enricher/    enricher Lambda entry point
  scheduler/   scheduler Lambda entry point
  fc-init/     guest init process (runs as PID 1 inside each VM)
internal/
  agent/       reconciliation loop, metrics publishing
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
examples/      example agent configs and enricher inputs
scripts/       smoke-local.sh
docs/          architecture and config reference
```

## Building

```bash
make help          # show all targets with descriptions
```

| Target | Output | When to use |
|--------|--------|-------------|
| `make build` | `bin/firework-agent` (native OS) | local testing, smoke test |
| `make build-linux-arm64` | `bin/firework-agent-linux-arm64` + `bin/fc-init` | updating a running node or Packer AMI build |
| `make build-fc-init` | `bin/fc-init` (Linux ARM64, static) | updating the guest init binary in a rootfs image |
| `make package-enricher` | `bin/enricher.zip` | deploying the enricher Lambda |
| `make package-scheduler` | `bin/scheduler.zip` | deploying the scheduler Lambda |
| `make build-all` | everything above | full release build |
| `make clean` | removes `bin/` and test cache | — |

`fc-init` is always built as a static Linux/ARM64 binary (`CGO_ENABLED=0`) because it runs inside the VM rootfs as PID 1. Lambda binaries are built with `-tags lambda.norpc` and packaged as `bootstrap` inside a ZIP.

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

## Iterating on Lambda Code

You do not need to rebuild the Packer AMI to update Lambda code. After changing enricher or scheduler code:

```bash
make package-enricher
aws lambda update-function-code \
  --function-name <enricher-function-name> \
  --zip-file fileb://bin/enricher.zip

make package-scheduler
aws lambda update-function-code \
  --function-name <scheduler-function-name> \
  --zip-file fileb://bin/scheduler.zip
```

Trigger the enricher manually (skipping the GitHub webhook) with an EventBridge test event or:

```bash
aws lambda invoke \
  --function-name <enricher-function-name> \
  --payload '{}' \
  /tmp/enricher-out.json && cat /tmp/enricher-out.json
```

## End-to-End Deployment Flow

See [firework-deployment-example](https://github.com/artemnikitin/firework-deployment-example) for Terraform and Packer sources. The high-level order:

1. **Build AMI** — run Packer with the agent and Traefik binaries baked in.
2. **Deploy control plane** — Terraform creates the enricher/scheduler Lambdas, S3 config bucket, API Gateway, and EventBridge rule.
3. **Deploy data plane** — Terraform creates the VPC, ALB, and Auto Scaling Group using the AMI from step 1.
4. **Configure GitHub webhook** — point the config repo's webhook at the API Gateway URL with the shared secret.
5. **Push a config** — a push to the config repo (or waiting up to 1 min for EventBridge) triggers the enricher, which writes `nodes/*.yaml` to S3. Agents pick it up on their next poll.

For IAM setup, refer to `iam-policies/` in the deployment repo. The policy for the deployment user has a ~6 KB size limit, so check coverage before adding new AWS resources to Terraform.

## Code Conventions

- Format with `gofmt -s` (`make fmt`).
- Tests are table-driven where multiple cases exist.
- AWS-dependent code is behind interfaces so unit tests can substitute fakes (see `store.Store`, `vm.Manager`).
- The scheduler and enricher pipelines are pure functions — prefer keeping them that way.
