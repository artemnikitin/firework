# <img src="logo.svg" alt="firework logo" height="48"/>

A lightweight, pull-based orchestrator for services running in [Firecracker](https://firecracker-microvm.github.io/) microVMs.

## Related Repositories

- [firework-deployment-example](https://github.com/artemnikitin/firework-deployment-example) - Terraform + Packer deployment on AWS
- [firework-gitops-example](https://github.com/artemnikitin/firework-gitops-example) - example GitOps input repo and rootfs image build pipeline

## What Firework Includes

- `firework-agent` (node runtime): pulls desired state from Git or S3 and reconciles local Firecracker VMs.
- `enricher` (Lambda): converts user-friendly service specs into resolved per-node configs.
- `scheduler` (Lambda): performs node placement using capacity information.

## How It Works

```mermaid
flowchart TD
  CFG[(Config store<br/>S3 or Git)] -->|poll| AGENT[firework-agent]
  IMG[(S3 images bucket)] -->|optional image sync| SYNC[Image syncer]
  AGENT --> SYNC
  AGENT --> REC[Reconcile loop]
  SYNC --> REC

  REC --> NET[Network manager<br/>bridge / TAP / iptables]
  REC --> VM[VM manager]
  VM --> FC[Firecracker microVMs]

  REC --> HC[Health monitor]
  HC -->|restart on repeated failures| VM

  REC --> TR[Traefik config manager]
  TR --> DYN[/Traefik dynamic config files/]

  AGENT --> API[Local API<br/>/healthz /health /status /metrics]
```

## Documentation

- Architecture: [`docs/architecture/README.md`](docs/architecture/README.md)
- Configuration reference: [`docs/configs/README.md`](docs/configs/README.md)
- Example agent configs: [`examples/`](examples/)

## Quick Start

Prerequisites:

- Linux host with KVM (`/dev/kvm`)
- Firecracker binary installed
- Go 1.25+ (for building from source)

Build binaries:

```bash
make build-all
```

Run agent with an example config:

```bash
sudo ./bin/firework-agent --config examples/agent-s3.yaml
```

## Deployment Modes

- **Recommended:** GitOps input -> enricher -> S3 `nodes/*.yaml` -> agent polls S3.
- **Simple mode:** agent pulls fully resolved `nodes/*.yaml` directly from Git (no enricher/scheduler).

## License

MIT
