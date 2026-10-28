# Guest Lifecycle and Persistent Volumes

## Guest ownership

`fc-init` remains the guest's PID 1 while the application runs. It performs
privileged guest setup and starts the application as a child with the configured
credentials, environment, argv, and working directory. PID 1 forwards external
signals to the application process group and reaps exited descendants. With no
application arguments, `fc-init` still replaces itself with `/sbin/init`.

The Linux-only guest lifecycle server accepts one bounded, versioned `quiesce`
operation on AF_VSOCK port 4050. The host connects to Firecracker's configured
Unix socket using Firecracker's `CONNECT 4050\n` handshake before sending the
length-prefixed lifecycle request. An older guest with no listener is treated
as unavailable; ordinary stop uses the existing host termination path.

## Stop sequence

For a tracked VM, `Manager.Stop` serializes lifecycle operations for that
service, publishes `StateStopping`, captures its immutable manifest identity,
and releases the global manager mutex. The host verifies ownership before the
guest request, waits for the bounded response, and revalidates the same manager
instance and process identity before each Firecracker signal. Heartbeats and
status readers do not wait for guest shutdown.

On guest success, PID 1 has observed the application exit, called `sync`, and
checked every persistent mount while unmounting in reverse order. The host
requires the response's volume names and paths to match the ownership manifest,
and persists the quiesce confirmation before stopping Firecracker. A quiesce
failure on an ordinary stop is surfaced as a volume error when the VM has
persistent volumes, then termination continues only after ownership checks.

This is graceful termination. Firecracker is stopped and a later start creates
a new guest. The channel does not capture or restore process memory and must not
be described as a reversible snapshot.

## Resize sequence

Reconciliation performs volume preflight and settles advisory refusals before
deciding whether a resize is effective. It requires `RemoveForResize` only
when an existing volume's accepted effective `size_bytes` differs from the
running config. A no-op or a rejected shrink does not invoke this path.

`RemoveForResize` fails closed if the guest does not confirm application exit,
filesystem sync, and every expected unmount. It leaves a still-owned old VM
running and returns before the backing image is removed or resized. Ordinary
stop keeps its forced-termination fallback for old guest images and timed-out
workloads; that fallback is not sufficient evidence to resize a volume.

`guest_quiesce_timeout` defaults to `15s` and is bounded to `1s` through `5m`.
Existing agent YAML files inherit the default. A custom timeout must be wired
into each deployed agent config. The guest image must contain the matching
`fc-init`, and the guest kernel must enable `CONFIG_VIRTIO_VSOCKETS=y`.
`firework-gitops-example` compiles its bundled guest-init source by default, so
that source must be updated or image builds must select a new core release.
