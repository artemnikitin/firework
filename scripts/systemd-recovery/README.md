# Systemd recovery integration checks

Run `./scripts/systemd-recovery/run.sh` from a checkout with Go and Docker.
The script builds a disposable Linux image, starts real systemd with private
cgroups, and runs recovery tests three times. Its privileged container creates
TAP devices in its own network namespace. No host bind mounts are configured. Privileged mode grants broad access to the
Docker host, so use a trusted disposable Docker environment (such as Docker
Desktop). The container is removed on exit. Docker image layers remain
cached locally.

The payload implements Firecracker's launch arguments and Unix socket ownership
but does not boot a guest. The tests exercise real systemd units, Linux process
identity, PID-zero recovery, replacement after a failed terminal manifest write,
and kernel TAP retention while ownership remains ambiguous. A separate kernel
probe confirms that deleting and recreating a same-name TAP does not reconnect
the old descriptor; retaining the existing device is essential. They require no
cloud credentials or KVM. They do not establish guest boot, quiesce or persistent
volume integrity.

These opt-in tests use the `systemd_integration` build tag and
`FIREWORK_REAL_SYSTEMD=1`; ordinary `go test ./...` does not run them. Run them
only in an isolated systemd environment, as they create transient units and a
TAP named `tap-pr66-probe`.
