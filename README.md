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
        GITHUB["GitHub\nConfig Repo\n(services/*.yaml)"]
        CI["CI / CD\n(builds rootfs images)"]
    end

    subgraph control_plane["AWS – Control Plane"]
        APIGW["API Gateway\nPOST /webhook"]
        EB["EventBridge\nevery 1 min"]
        ENRICHER["Enricher Lambda\ntransforms specs → node configs"]
        SCHED["Scheduler Lambda\nbin-pack placement"]
        S3CFG["S3 Config Bucket\nnodes/&lt;node&gt;.yaml"]
        CW["CloudWatch\nMetrics · Logs · Dashboards"]
    end

    S3IMG["S3 Images Bucket\n(rootfs images, kernels)"]

    subgraph vpc["AWS VPC – Data Plane"]
        ALB["Application Load Balancer\nHTTPS · wildcard TLS · multi-AZ"]

        subgraph node1["EC2 Node 1  (c6g.metal, private subnet)"]
            AG1["firework-agent"]
            TR1["Traefik"]
            MV1A["Firecracker VM: svc-a\n(fc-init + app)"]
            MV1B["Firecracker VM: svc-b\n(fc-init + app)"]
        end

        subgraph node2["EC2 Node 2  (c6g.metal, private subnet)"]
            AG2["firework-agent"]
            TR2["Traefik"]
            MV2A["Firecracker VM: svc-c\n(fc-init + app)"]
        end
    end

    USER -->|HTTPS :443| ALB
    ALB -->|HTTP :8080| TR1
    ALB -->|HTTP :8080| TR2
    TR1 --> MV1A & MV1B
    TR2 --> MV2A
    TR1 <-.->|"cross-node proxy (remote peer routes)"| TR2

    GITHUB -->|push webhook| APIGW
    APIGW --> ENRICHER
    EB --> ENRICHER
    ENRICHER -->|invoke| SCHED
    CW -->|capacity metrics| SCHED
    SCHED -->|node assignments| ENRICHER
    ENRICHER -->|"write nodes/node1.yaml, nodes/node2.yaml"| S3CFG

    CI -->|upload| S3IMG

    AG1 -->|"poll every 30s"| S3CFG
    AG2 -->|"poll every 30s"| S3CFG
    AG1 -->|pull missing images| S3IMG
    AG2 -->|pull missing images| S3IMG
    AG1 -->|publish capacity metrics| CW
    AG2 -->|publish capacity metrics| CW
    AG1 -->|manages routes| TR1
    AG2 -->|manages routes| TR2
    AG1 -->|reconciles| MV1A & MV1B
    AG2 -->|reconciles| MV2A
```

## Documentation

- Architecture overview: [`docs/architecture/README.md`](docs/architecture/README.md)
- Design decisions and rationale: [`docs/architecture/DESIGN.md`](docs/architecture/DESIGN.md)
- Configuration reference: [`docs/configs/README.md`](docs/configs/README.md)
- Example agent configs: [`examples/`](examples/)
- Development guide: [`DEVELOPMENT.md`](DEVELOPMENT.md)

## License

MIT
