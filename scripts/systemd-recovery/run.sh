#!/usr/bin/env bash
set -euo pipefail

# Real systemd, /proc and TAP validation in an isolated disposable container.
# No host bind mounts are configured. Use a trusted disposable Docker environment.
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_dir="$(mktemp -d)"
container_name="firework-systemd-test-$$"
cleanup() {
  docker rm -f "$container_name" >/dev/null 2>&1 || true
  rm -rf "$test_dir"
}
trap cleanup EXIT
image_name="firework-systemd-recovery-test:local"
docker build -t "$image_name" "$root_dir/scripts/systemd-recovery"
architecture="$(docker image inspect "$image_name" --format '{{.Architecture}}')"
cd "$root_dir"
GOOS=linux GOARCH="$architecture" go test -c -tags systemd_integration -o "$test_dir/vm.test" ./internal/vm
docker run -d --name "$container_name" --privileged --cgroupns=private \
  --tmpfs /run --tmpfs /run/lock "$image_name" >/dev/null
ready=false
for attempt in {1..60}; do
  state="$(docker exec "$container_name" systemctl is-system-running 2>/dev/null || true)"
  if [[ "$state" == running || "$state" == degraded ]]; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  docker logs "$container_name"
  exit 1
fi
# /tmp may be replaced during systemd boot; keep the binary in /root.
docker cp "$test_dir/vm.test" "$container_name:/root/vm.test"
docker exec "$container_name" /usr/lib/systemd/systemd --version
docker cp "$root_dir/scripts/systemd-recovery/tap-fd.py" "$container_name:/root/tap-fd.py"
docker exec "$container_name" python3 /root/tap-fd.py
docker exec -e FIREWORK_REAL_SYSTEMD=1 "$container_name" \
  /root/vm.test -test.run '^TestSystemdIntegration' -test.v -test.count=3
