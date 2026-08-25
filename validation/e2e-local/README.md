# Local stateful two-node E2E validation

This harness runs one combined Firework setup inside an arm64 Lima Linux
guest on Apple Silicon:

```text
one control plane (registry + events + controller + API)
                         |
                 real per-run S3 bucket
                         |
       node-a namespace ---------------- node-b namespace
       firework-agent                    firework-agent
       Firecracker microVMs              Firecracker microVMs
```

The workload uses the arm64 rootfs images produced by
`firework-gitops-example`: Elasticsearch and Kibana start together on node-a,
then the same desired state is changed to anti-affine placement and Kibana
moves to node-b. Both phases require eventual `running`/`healthy` convergence;
short startup or movement downtime is expected and allowed. Elasticsearch is
also driven to cluster status `green` after its single-node replica setting is
adjusted for this validation.

The run additionally checks the real S3 state/rendered-config path, local
volume creation and reuse, agent restart/adoption, explicit empty desired
state, stale-node visibility and recovery, cross-node routing, and final
per-node port ownership. CI, Linux-native execution, and hosted-KVM probing
are Milestone 2 work and are not required by this local command today.

## Prerequisites

- Apple Silicon macOS with Lima 2.x and a Lima `vz` guest that exposes
  readable/writable `/dev/kvm` and `/dev/net/tun`.
- Go and the repository build toolchain.
- AWS credentials able to create/list/write/delete a disposable S3 bucket and
  read the workload-image bucket.
- Access to the existing arm64 GitOps image bucket. The default is
  `artemnikitin-firework-images`; override it with
  `FIREWORK_E2E_IMAGES_BUCKET` when needed.

The harness downloads and verifies Firecracker 1.12.0 arm64 and uses the pinned
Firecracker CI kernel `firecracker-ci/v1.12/aarch64/vmlinux-5.10.233`. The VM
configuration enables Firecracker's VirtIO-RNG device so Java/Node workloads
do not wait indefinitely for guest entropy. Override either asset pin only for
an intentional compatibility investigation. The two workload rootfs images
are not copied into the repository or manually modified: the agents download
them through their production S3 image-sync path.

## Run

```bash
export AWS_REGION=us-east-1
export FIREWORK_E2E_AWS_PROFILE=artemnikitin
make validate-e2e-local
```

Useful local options:

- `FIREWORK_E2E_KEEP=1` retains the Lima guest, logs, manifest and disposable
  S3 bucket for inspection. Clean it with
  `make validate-e2e-local-clean FIREWORK_E2E_MANIFEST=<manifest>`.
- `FIREWORK_E2E_TIMEOUT=1800` changes the bounded scenario timeout. The default
  is intentionally generous because these production-sized rootfs images can
  take several minutes to initialize under nested virtualization.
- `FIREWORK_E2E_LIMA_CPUS=8`, `FIREWORK_E2E_LIMA_MEMORY_GB=12`, and
  `FIREWORK_E2E_LIMA_DISK_GB=60` size the local guest.
- By default Elasticsearch gets 4 vCPUs and 6 GiB, while Kibana gets 2 vCPUs
  and 4 GiB. Override these with `FIREWORK_E2E_ES_VCPUS`,
  `FIREWORK_E2E_ES_MEMORY_MB`, `FIREWORK_E2E_KIBANA_VCPUS`, and
  `FIREWORK_E2E_KIBANA_MEMORY_MB` when the host has different capacity.
- `FIREWORK_E2E_HEALTH_RETRIES=80` controls the startup/restart threshold.
  `FIREWORK_E2E_ES_JAVA_OPTS=-Xmx1g` is the compatibility default for the
  currently published GitOps Elasticsearch image; a rebuilt image with the
  current `fc-init` can use a normal multi-option value.
- `FIREWORK_E2E_VOLUME_SIZE=2Gi` and
  `FIREWORK_E2E_STORAGE_CAPACITY=8Gi` adjust the disposable local volume
  pool.
- `FIREWORK_E2E_ES_IMAGE_KEY` and `FIREWORK_E2E_KIBANA_IMAGE_KEY` select
  alternate objects with the same GitOps rootfs contract.
- `FIREWORK_E2E_FIRECRACKER_BIN` and `FIREWORK_E2E_KERNEL` optionally provide
  local asset overrides; otherwise the pinned downloads are used.

The runner creates a unique real S3 bucket for control-plane state and
rendered node configs, records image/asset provenance, collects diagnostics
before teardown, and deletes the bucket and Lima guest unless retention is
requested. AWS credentials are passed through the process environment and are
not written to the manifest or generated configuration files.
