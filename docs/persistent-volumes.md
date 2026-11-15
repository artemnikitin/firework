# Persistent Volumes

Firework keeps application data separate from the writable root filesystem by
attaching one quota-sized ext4 image per declared volume.

## Application configuration

```yaml
volumes:
  - name: data
    type: local
    mount_path: /var/lib/application
    size: 20Gi
```

`size` is optional. Resolution uses the explicit value, then the matching
`volume_defaults.local_size` or `volume_defaults.shared_size`, then `10Gi`.
The resolved quota is a hard filesystem maximum; ext4 metadata makes usable
space slightly smaller. Only positive integer `Mi` and `Gi` values are
accepted. A service may declare at most 25 volumes (`/dev/vdb` through
`/dev/vdz`), and Firework rejects a generated guest payload that would exceed
the portable kernel command-line limit.

`local` volumes are bound permanently to the node where their durable record
was created. If that node is unavailable, the service remains pending instead
of receiving an empty volume elsewhere. Removing a service retains its record
and data.

The control plane fills `bound_node` in rendered node config from its durable
record. In direct Git mode, an operator must add the physical node's stable
identity to `bound_node` in the resolved node config; the agent rejects a
missing or mismatched binding rather than treating a reusable node label as
storage identity.

`shared` is part of the provider-neutral schema and deployment contract, but
runtime placement remains pending until the EFS/Filestore partition-fencing
gates in [issue #32](https://github.com/artemnikitin/firework/issues/32) are
complete. The durable per-VM supervisor preserves ownership across agent
restarts, but that alone cannot prove exclusive access during cloud filesystem
partitions, so Firework continues to fail closed for shared volumes.

## Agent configuration

The operator provisions and mounts storage before starting the agent:

```yaml
storage:
  local:
    path: /var/lib/firework/volumes
    capacity: 500Gi
  shared:
    backend_id: primary
    path: /mnt/firework-shared
    capacity: 1Ti
```

Configured paths must be actual mount points. `capacity` is a logical admission
budget and retained volumes continue to reserve it. Firework creates sparse
ext4 images with no root-reserved percentage, but also checks host free space
before creation or growth.

## Resize and retention

Quota changes are offline. Before stopping an existing VM, Firework validates
identity, binding, capacity, filesystem health, and shrink feasibility. It then
records a durable resize transaction, grows the file before ext4, or shrinks
ext4 before truncating the file. Ambiguous states fail closed.

### The agent's service unit must not kill filesystem utilities

Creating, checking, and resizing a volume runs `mkfs.ext4`, `e2fsck`, and
`resize2fs` as child processes. Interrupting one mid-operation is what the
resize transaction and `e2fsck -f -y` exist to recover from, and the agent
therefore detaches those commands from its own shutdown: they keep running when
the agent's context is cancelled, with their own timeout and a SIGTERM-then-wait
cancellation rather than an immediate kill.

**That protection is defeated by the default systemd supervision.** With
`KillMode=control-group`, systemd's default, stopping or restarting the unit
signals *every* process in its control group, including the detached utility,
and escalates to SIGKILL for the whole group once `TimeoutStopSec` expires. A
node drain during a shrink then kills `resize2fs` exactly as if the agent had
never detached it.

A unit running `firework-agent` must therefore set:

```ini
[Service]
# SIGTERM reaches only the agent, so a detached utility keeps running.
KillMode=mixed
# Must exceed the agent's destructive-command deadline (30m) plus its grace
# period, or systemd force-kills the utility at the timeout.
TimeoutStopSec=2100
```

The two numbers are a contract: `destructiveCommandTimeout` in
`internal/volume` bounds the command, and `TimeoutStopSec` must stay above it.
Raising one without the other reopens the gap.

### Guest quiesce before stopping a microVM

The guest `fc-init` remains root as PID 1 and starts the application as a child
with the configured user, primary group, argv, environment and working directory.
Directory setup happens only in the child, while privileged, before dropping
credentials; PID 1 never holds an application volume as its working directory.
It forwards signals to the immediate child, reaps adopted descendants, and listens
for a versioned lifecycle request over Firecracker vsock. Before sync and
reverse-order unmount, every application descendant must exit and be reaped.
Each unmount result is checked before the host treats quiesce as successful.

`guest_application_shutdown_timeout` covers application shutdown and reaping.
`guest_flush_timeout` starts afterwards and covers sync plus every unmount
together. Each defaults to `15s` and accepts `1s` through `5m`. The total host
quiesce wait is their sum plus 15s for transport; existing VMM termination timers
are separate. Existing agent configs inherit both defaults. Custom values must be
wired into deployed configs. The old branch-only `guest_quiesce_timeout` key is
rejected. A timeout cannot cancel a blocking guest sync/unmount syscall; only one
flush job runs at a time, and an expired response cannot authorize resize.

For an ordinary stop or service update, a failed or unavailable guest request
is reported and the host continues with the existing ownership-validated
Firecracker termination fallback. That fallback can still lose dirty guest
data. For an accepted volume resize, Firework requires confirmed guest
quiesce before removing the old VM or changing its volume image. If quiesce
fails while the VMM remains live, its actual config and resource claims are
retained and no host resize, truncation or offline filesystem modification occurs.
Guest writes and sync can still change the image contents. The revision stays
incomplete with a named `guest_quiesce_required` service hold. The identical
revision retries on the next reconcile while unrelated services can progress.
A rejected shrink and a no-op effective size do not enter this path.

The application may already have exited, and some mounts may already be absent.
The guest remains available for cleanup retry; the refusal does not restart the
app, remount volumes or force VM replacement. Cancelling or changing the resize
clears its old attempt reason without claiming that the app recovered. A known
application exit continues to suppress readiness for that VM. Existing health
restarts use the prior actual config and sizes; without a working health check,
operator action may be needed. An agent restart relies on ordinary ownership
recovery and a fresh request, without persisted quiesce proof. A VM proven stopped
or failed can resize on its next start through the existing offline filesystem
checks, without a previous quiesce confirmation.

Older guest images without the lifecycle listener remain stoppable through
the ordinary fallback but cannot pass the resize-required quiesce check. Ship
the agent first, then images with the new guest `fc-init`, then request size
changes in a later revision. Restart VMs on those images first; combining image
and size changes still holds the old guest.
Firework's `firework-gitops-example` builds a bundled `scripts/fc-init/main.go`
by default, so the [companion port](https://github.com/artemnikitin/firework-gitops-example/issues/34)
must include protocol version 2 and both timeout fields, or the build must select
a pinned core binary through `FC_INIT_BIN`. Updating the core agent alone does not update an already published
guest rootfs.

Quiesce terminates the workload and stops the VM; it is not a reversible,
process-preserving snapshot. Applications still need to handle termination
signals and keep their own transaction guarantees for abrupt host failure,
kernel failure, timeouts, or old guest images. The guest kernel must enable
`CONFIG_VIRTIO_VSOCKETS=y`, and the host must support Firecracker's vsock
device.

### Refused size requests

A size request the cluster cannot serve is refused rather than applied, and the
service keeps running at the size it already has. Two cases produce this:

- the bound node's pool has no room for the larger reservation, or its capacity
  cannot be verified because the node is not currently active;
- the requested shrink is below the safe minimum for the filesystem's current
  contents.

A refusal is terminal for that request: it is recorded durably, no further
resize is attempted, and the service is neither stopped nor restarted. Status
therefore carries three sizes rather than two — **requested** (what the repo
asked for), **effective** (what the control plane accepted and rendered), and
**applied** (what exists on disk) — and the API, `fireworkctl service <name>`,
and the UI show requested next to effective. A refused request is flagged with
its refusal reason. Effective and applied sizes can also differ while an
accepted resize is still waiting to be applied.

To recover, change the requested size to something feasible. That is a new
request, so it is measured and admitted from scratch. Reverting the request to
the effective size withdraws it: the refusal stops being reported and no resize
is performed.

While a refusal stands for a volume the desired revision still declares, the
deployment status reports the revision `degraded` with reason
`volume_size_rejected` rather than converged — a cluster running a size nobody
asked for is not converged, even though every workload is healthy. Deleting the
service ends that: its record is retained, but nothing is asking for the size
any more.

### Direct-Git resize generations

In control-plane-managed mode the controller mints a new `resize_generation`
whenever a volume's requested size changes, so a corrected request is always
distinguishable from a repeat of a refused one.

Hand-authored node configs (see [docs/configs](configs/README.md)) carry
`resize_generation` themselves, and Firework cannot mint one for them.
**Bump `resize_generation` whenever you change a volume's `size_bytes`** to
identify each request explicitly. The runtime compares both size and
generation, so changing the size can trigger growth or shrink even when the
generation is unchanged, zero, or omitted. A corrected size at an unchanged
generation is re-measured rather than inheriting a refusal for a different
size; reverting to the size already running withdraws the request at any
generation.

`configcheck --node-config <file>` validates the config and warns when it
declares a volume size with an absent or zero generation. That warning is
advisory: the checker has no previous revision with which to verify a generation
bump.

Deleting application YAML never deletes a volume. Permanent deletion is a
manual operator action: first stop/remove the service placement, back up the
volume if required, then remove both its host directory and
`cp/v1/volumes/<service>/<volume>.json`. Never delete only one side and expect
automatic adoption.

Service detail in the UI, API, and `fireworkctl service <name>` includes the
logical ID, binding/backend, requested/effective/applied quota, resize
generation, and preparation state.
