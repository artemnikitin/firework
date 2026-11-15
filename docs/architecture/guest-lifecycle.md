# Guest Lifecycle and Persistent Volumes

## Guest ownership

`fc-init` remains the guest's PID 1 while the application runs. It performs
privileged guest setup and starts the application as a child with the configured
credentials, environment, argv, and working directory. Only the child changes
directory, while still root, before preparing ownership and dropping credentials.
Directory-entry failure warns and retains the original directory. Executable
lookup happens after the credential drop, using main's fixed search list and
relative-path fallback. Named users receive their primary group only; an empty
user retains the inherited credentials and account environment. Setup descriptors
close before the final exec. PID 1 stays off application volume mounts.

PID 1 forwards each external signal to the immediate application child. A wrapper
such as tini retains its argv and forwards to its own children. Once the parent
exits, PID 1 signals remaining adopted children and reaps them. It never combines
process-group and individual sends for the same signal. An unrequested app exit
retains its exit status and fails the guest as before, without a filesystem flush.
With no application arguments, `fc-init` still replaces itself with `/sbin/init`.

The Linux-only guest lifecycle server accepts one bounded, versioned `quiesce`
operation on AF_VSOCK port 4050. The host connects to Firecracker's configured
Unix socket using Firecracker's `CONNECT 4050\n` handshake before sending the
length-prefixed JSON lifecycle request. Protocol version 2 carries independent
`application_timeout_ms` and `flush_timeout_ms` fields, each bounded to 1ms through
5m. Frames, mount counts, paths and errors are bounded. A mismatched peer fails
closed for resize; ordinary stop uses the existing host termination path.

## Stop sequence

For a tracked VM, `Manager.Stop` uses main's lifecycle reservation and ownership
checks, publishes `StateStopping`, captures its immutable manifest identity,
and releases the manager mutex. No additional per-service lock is added. The host
verifies ownership before the
guest request, waits for the bounded response, and revalidates the same manager
instance and process identity before each Firecracker signal. Heartbeats and
status readers do not wait for guest shutdown.

On guest success, PID 1 has reaped the application and all descendants, called
`sync`, and checked every persistent mount while unmounting in reverse order. The host
requires the response's volume names and paths to match the ownership manifest
before stopping Firecracker. Confirmation belongs only to this operation and
is never persisted. A quiesce
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

`RemoveForResize` fails closed if a live guest does not confirm application exit,
filesystem sync, and every expected unmount. It retains the VMM, its actual config
and resource claims, and performs no host resize, truncation or offline filesystem
modification. Guest writes and sync may still alter image contents. Ordinary
stop keeps its forced-termination fallback for old guest images and timed-out
workloads; that fallback is not sufficient evidence to resize a volume.

The reconciler records a bounded in-memory last-attempt reason against the
immutable VM instance ID and volume request, including resize generation. Status
reports `guest_quiesce_required` within the service's resource hold. This result
does not suppress planning: an identical desired revision retries, other services
can update, and the revision stays incomplete. Claims still in use by this VM
remain protected from conflicting updates and deletes. Success, request withdrawal
or change, removal and replacement clear the applicable attempt result.

A failed request may already have stopped the application or unmounted some
volumes. PID 1 stays available for another request; retries skip absent mounts.
There is no automatic application restart, remount or forced VM replacement for
this refusal. A separately tracked observation of application exit prevents stale
passing health data, or withdrawal alone, from making that same VM ready again.
Normal health restarts use the previous actual config and sizes. Without a working
health check, operator action may be required.

After an agent restart, main's recovery policy still re-adopts a validated live
VM, including a recorded `stopping` VM. The next resize asks again. A VM proven
not running can be removed and resized on the next start without historical
quiesce proof, using the existing offline `e2fsck`/`resize2fs` path.

## Timeouts and rollout

The agent settings `guest_application_shutdown_timeout` and `guest_flush_timeout`
each default to `15s` and accept `1s` through `5m`. The first covers signals,
application exit and descendant reaping. The second begins afterwards and covers
sync plus all unmounts together. The old branch-only `guest_quiesce_timeout`
setting is rejected with a migration error.

The host shares one transport deadline across connection, handshake, request and
reply, bounded by the two configured budgets plus 15s. Firecracker's existing
TERM/KILL timers run separately. Deadline expiry cannot cancel a blocked guest
sync/unmount syscall. The guest keeps at most one flush job active, lets retries
join it, and rejects expired operations; a late reply or replaced VM cannot
authorize host resize. A retry after completed partial cleanup starts fresh.

Ship the agent first, then rebuilt guest images, then size changes in a separate
revision. Each VM needs a restart with the vsock device and the matching `fc-init`.
An image and size change in one revision remains held on an old guest. The guest
kernel must enable `CONFIG_VIRTIO_VSOCKETS=y`.
`firework-gitops-example` compiles its bundled `scripts/fc-init/main.go`; its
[companion port](https://github.com/artemnikitin/firework-gitops-example/issues/34)
must include protocol version 2 and both timeout fields. Updating core alone does
not update published images. Image builds can select a pinned core binary through
`FC_INIT_BIN`.

Local fake-based tests, the real Unix listener round trip and systemd container
checks do not boot a guest. Both-architecture real Firecracker checks, including
child workdir permissions and a workdir inside a volume, remain the owner's
manual post-merge batch in the
[issue #49 contract](https://github.com/artemnikitin/firework/issues/49#issuecomment-6800673129).
