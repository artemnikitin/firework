#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SMOKE_API_PORT="${SMOKE_API_PORT:-18081}"
POLL_INTERVAL="${POLL_INTERVAL:-2s}"
KEEP_SMOKE_TMP="${KEEP_SMOKE_TMP:-0}"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/firework-smoke.XXXXXX")"
CONFIG_REPO="${WORKDIR}/config-repo"
STATE_DIR="${WORKDIR}/state"
AGENT_CFG="${WORKDIR}/agent.yaml"
AGENT_BIN="${WORKDIR}/firework-agent"
AGENT_LOG="${WORKDIR}/agent.log"
FAKE_FIRECRACKER_BIN="${WORKDIR}/fake-firecracker"
FAKE_FIRECRACKER_LOG="${WORKDIR}/fake-firecracker.log"
AGENT_PID=""

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  local status=$?
  if [[ -n "${AGENT_PID}" ]] && kill -0 "${AGENT_PID}" >/dev/null 2>&1; then
    kill "${AGENT_PID}" >/dev/null 2>&1 || true
    wait "${AGENT_PID}" >/dev/null 2>&1 || true
  fi

  if [[ "${KEEP_SMOKE_TMP}" == "1" ]]; then
    log "keeping smoke workspace at ${WORKDIR}"
  else
    rm -rf "${WORKDIR}"
  fi

  exit "${status}"
}
trap cleanup EXIT INT TERM

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    die "missing required command: $1"
  fi
}

wait_for_http() {
  local url="$1"
  local timeout_seconds="$2"
  local deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    if curl --silent --show-error --fail "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_log_pattern() {
  local pattern="$1"
  local file="$2"
  local timeout_seconds="$3"
  local deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    if [[ -f "${file}" ]] && grep -q "${pattern}" "${file}"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

write_node_config_v1() {
  cat > "${CONFIG_REPO}/nodes/dev.yaml" <<'EOF'
node: "dev"
services:
  - name: "demo"
    image: "/var/lib/images/demo.ext4"
    kernel: "/var/lib/images/vmlinux-5.10"
    vcpus: 1
    memory_mb: 128
    kernel_args: "console=ttyS0 reboot=k panic=1 pci=off"
EOF
}

write_node_config_v2() {
  cat > "${CONFIG_REPO}/nodes/dev.yaml" <<'EOF'
node: "dev"
services:
  - name: "demo"
    image: "/var/lib/images/demo.ext4"
    kernel: "/var/lib/images/vmlinux-5.10"
    vcpus: 1
    memory_mb: 128
    kernel_args: "console=ttyS0 reboot=k panic=1 pci=off"
  - name: "demo2"
    image: "/var/lib/images/demo2.ext4"
    kernel: "/var/lib/images/vmlinux-5.10"
    vcpus: 1
    memory_mb: 128
    kernel_args: "console=ttyS0 reboot=k panic=1 pci=off"
EOF
}

commit_config() {
  local message="$1"
  git -C "${CONFIG_REPO}" add nodes/dev.yaml
  git -C "${CONFIG_REPO}" \
    -c user.name="firework-smoke" \
    -c user.email="firework-smoke@local" \
    commit -m "${message}" >/dev/null
}

require_cmd git
require_cmd go
require_cmd curl

log "preparing smoke test workspace"
mkdir -p "${CONFIG_REPO}/nodes" "${STATE_DIR}"

if ! git -C "${CONFIG_REPO}" init -b main >/dev/null 2>&1; then
  git -C "${CONFIG_REPO}" init >/dev/null
  git -C "${CONFIG_REPO}" checkout -b main >/dev/null
fi

cat > "${FAKE_FIRECRACKER_BIN}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
log_file="${FAKE_FIRECRACKER_LOG_FILE:?FAKE_FIRECRACKER_LOG_FILE is required}"
printf '%s firecracker %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >> "${log_file}"
trap 'exit 0' INT TERM
while true; do
  sleep 1
done
EOF
chmod +x "${FAKE_FIRECRACKER_BIN}"

write_node_config_v1
commit_config "initial node config"

cat > "${AGENT_CFG}" <<EOF
node_name: "dev"
store_type: "git"
store_url: "file://${CONFIG_REPO}"
store_branch: "main"
poll_interval: "${POLL_INTERVAL}"
firecracker_bin: "${FAKE_FIRECRACKER_BIN}"
state_dir: "${STATE_DIR}"
log_level: "debug"
api_listen_addr: ":${SMOKE_API_PORT}"
enable_health_checks: false
enable_network_setup: false
EOF

log "building firework-agent"
(cd "${REPO_ROOT}" && go build -o "${AGENT_BIN}" ./cmd/agent)

log "starting firework-agent on port ${SMOKE_API_PORT}"
FAKE_FIRECRACKER_LOG_FILE="${FAKE_FIRECRACKER_LOG}" \
  "${AGENT_BIN}" --config "${AGENT_CFG}" >"${AGENT_LOG}" 2>&1 &
AGENT_PID="$!"

API_URL="http://127.0.0.1:${SMOKE_API_PORT}"
if ! wait_for_http "${API_URL}/healthz" 30; then
  die "agent API did not become healthy (see ${AGENT_LOG})"
fi

if ! wait_for_log_pattern "demo" "${FAKE_FIRECRACKER_LOG}" 30; then
  die "demo service did not start (see ${FAKE_FIRECRACKER_LOG})"
fi

log "updating desired config and waiting for reconciliation"
write_node_config_v2
commit_config "add demo2 service"

if ! wait_for_log_pattern "demo2" "${FAKE_FIRECRACKER_LOG}" 45; then
  die "demo2 service did not start after config update (see ${FAKE_FIRECRACKER_LOG})"
fi

status_json="$(curl --silent --show-error --fail "${API_URL}/status")"
if ! grep -q '"name": "demo2"' <<<"${status_json}"; then
  die "agent status does not include demo2 service"
fi

log "smoke test passed"
