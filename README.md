# <img src="logo.svg" alt="firework logo" height="48"/>

A lightweight orchestrator for [Firecracker](https://firecracker-microvm.github.io/) microVMs.

## Related Repositories

You can use them to have everything working end-to-end:

- [firework-deployment-example](https://github.com/artemnikitin/firework-deployment-example) - Terraform + Packer deployment on AWS
- [firework-gitops-example](https://github.com/artemnikitin/firework-gitops-example) - example GitOps input repo and rootfs image build pipeline

## How It Works

```mermaid
flowchart TB
    subgraph external["External"]
        USER["End Users"]
        GITHUB["GitHub\nConfig Repo"]
        CI["CI / CD"]
    end

    subgraph control_plane["Firework Control Plane"]
        REG["registry role\n(enroll/register/heartbeat)"]
        EV["events role\n(GitHub webhook)"]
        CTRL["controller role\n(schedule + publish)"]
    end

    subgraph storage["S3"]
        S3STATE["cp/v1 state"]
        S3CFG["nodes/*.yaml"]
        S3IMG["images bucket"]
    end

    subgraph data_plane["Firework Nodes"]
        N1["Node 1\nfirework-agent · Firecracker VMs"]
        N2["Node 2\nfirework-agent · Firecracker VMs"]
    end

    USER -->|HTTPS| N1 & N2
    GITHUB -->|push webhook| EV
    EV --> S3STATE
    REG --> S3STATE
    CTRL --> S3STATE
    CTRL -->|rendered configs| S3CFG
    CI -->|upload| S3IMG
    N1 & N2 -->|mTLS register + heartbeat| REG
    N1 & N2 -->|poll configs| S3CFG
    N1 & N2 -->|pull images| S3IMG
```

## Documentation

- Architecture overview: [`docs/architecture/README.md`](docs/architecture/README.md)
- Design decisions and rationale: [`docs/architecture/DESIGN.md`](docs/architecture/DESIGN.md)
- Configuration reference: [`docs/configs/README.md`](docs/configs/README.md)
- Example agent configs: [`examples/`](examples/)
- Development guide: [`DEVELOPMENT.md`](DEVELOPMENT.md)

## License

MIT
