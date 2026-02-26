# <img src="logo.svg" alt="firework logo" height="48"/>

A lightweight, pull-based orchestrator for services running in [Firecracker](https://firecracker-microvm.github.io/) microVMs.

## Related Repositories

You can use them to have everything working end-to-end:

- [firework-deployment-example](https://github.com/artemnikitin/firework-deployment-example) - Terraform + Packer deployment on AWS
- [firework-gitops-example](https://github.com/artemnikitin/firework-gitops-example) - example GitOps input repo and rootfs image build pipeline

## How It Works

The diagram below shows an example deployment: two EC2 bare-metal nodes running inside a private VPC, fronted by an ALB, with a GitOps-driven control plane built from two Lambdas, an S3 config bucket, and CloudWatch for observability.

```mermaid
flowchart TB
    subgraph external["External"]
        USER["End Users"]
        GITHUB["GitHub\nConfig Repo"]
        CI["CI / CD"]
    end

    subgraph control_plane["AWS – Control Plane"]
        APIGW["API Gateway"]
        EB["EventBridge"]
        ENRICHER["Enricher Lambda"]
        SCHED["Scheduler Lambda"]
        S3CFG["S3 Config Bucket"]
        CW["CloudWatch"]
    end

    S3IMG["S3 Images Bucket"]

    subgraph vpc["AWS VPC – Data Plane"]
        ALB["Application Load Balancer\n(public, multi-AZ)"]

        subgraph node1["EC2 Node 1  (c6g.metal, private subnet)"]
            N1["firework-agent\nFirecracker VMs"]
        end

        subgraph node2["EC2 Node 2  (c6g.metal, private subnet)"]
            N2["firework-agent\nFirecracker VMs"]
        end
    end

    USER -->|HTTPS| ALB
    ALB --> N1 & N2

    GITHUB -->|push webhook| APIGW
    APIGW --> ENRICHER
    EB -->|periodic trigger| ENRICHER
    ENRICHER -->|invoke| SCHED
    CW -->|capacity metrics| SCHED
    SCHED -->|node assignments| ENRICHER
    ENRICHER -->|write node configs| S3CFG

    CI -->|upload images| S3IMG

    N1 & N2 -->|poll configs| S3CFG
    N1 & N2 -->|pull images| S3IMG
    N1 & N2 -->|publish metrics| CW
```

## Documentation

- Architecture overview: [`docs/architecture/README.md`](docs/architecture/README.md)
- Design decisions and rationale: [`docs/architecture/DESIGN.md`](docs/architecture/DESIGN.md)
- Configuration reference: [`docs/configs/README.md`](docs/configs/README.md)
- Example agent configs: [`examples/`](examples/)
- Development guide: [`DEVELOPMENT.md`](DEVELOPMENT.md)

## License

MIT
