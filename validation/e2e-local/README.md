# Local two-node E2E validation

This harness runs one complete local Firework setup:

```text
one control-plane process (registry + events + controller + API)
                         |
                 real AWS S3 bucket
                         |
          node-a network namespace -- node-b network namespace
             firework-agent             firework-agent
                  |                            |
             Firecracker VM               Firecracker VM
```

The control plane reconciles a disposable local Git repository. It publishes
the desired state and rendered node configs into a unique real S3 bucket. Both
agents enroll over mTLS, consume their rendered configs from that bucket, and
run one Firecracker VM each. The caller VM is deliberately placed on the
opposite node from the responder VM and reaches it through a rendered
cross-node link.

This is local developer validation for now. CI and the unprivileged-build /
privileged-lab handoff are the next milestone.

## Prerequisites

- Linux with root, `/dev/kvm`, `/dev/net/tun`, `ip`, and `iptables`; or
  Apple Silicon macOS with Lima, nested virtualization, and a Linux guest
  that exposes `/dev/kvm`.
- Go and the repository build toolchain.
- AWS credentials with permission to create, list, write, and delete a
  disposable S3 bucket. The runner creates and deletes its own bucket on every
  run; it refuses to reuse an existing bucket name.
- A Linux Firecracker binary, an uncompressed Linux kernel, and an ext4
  rootfs containing BusyBox. The runner copies `fc-init` and the disposable
  E2E init script into a temporary copy of the rootfs, so the supplied rootfs
  is not modified.

Set these paths before running:

```bash
export FIREWORK_E2E_FIRECRACKER_BIN=/path/to/firecracker
export FIREWORK_E2E_KERNEL=/path/to/vmlinux
export FIREWORK_E2E_ROOTFS=/path/to/rootfs.ext4
export AWS_REGION=us-east-1
make validate-e2e-local
```

When `AWS_PROFILE` is used instead of environment credentials, the host
runner exports that profile's current credentials before entering Lima. This
also supports short-lived credentials obtained through the AWS CLI.

Useful local-only options:

- `FIREWORK_E2E_MODE=linux` forces native Linux execution.
- `FIREWORK_E2E_MODE=lima` forces the macOS Lima adapter.
- `FIREWORK_E2E_KEEP=1` retains the lab and prints a manifest path and cleanup
  command. Use `make validate-e2e-local-clean FIREWORK_E2E_MANIFEST=...` after
  inspection.
- `FIREWORK_E2E_TIMEOUT=600` changes the bounded scenario timeout.

The runner writes logs, redacted configuration diagnostics, S3 object
inventory, network state, and a run manifest under a temporary directory. AWS
credentials are passed to the runtime through the process environment and are
never written to the manifest or generated configuration files; the generated
files do contain short-lived local mTLS/bootstrap tokens needed by the lab.
