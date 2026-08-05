#!/usr/bin/env bash
set -Eeuo pipefail

RUN_DIR=${1:?usage: run-guest.sh RUN_DIR BUCKET REGION COMMIT MODE}
E2E_BUCKET=${2:?missing bucket}
AWS_REGION_VALUE=${3:?missing AWS region}
FIREWORK_COMMIT=${4:?missing Firework commit}
RUN_MODE=${5:?missing run mode}

export AWS_REGION="$AWS_REGION_VALUE"
export AWS_DEFAULT_REGION="$AWS_REGION_VALUE"
export AWS_EC2_METADATA_DISABLED=true

SCRIPT_DIR="${FIREWORK_E2E_SCRIPT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
BIN_DIR="$RUN_DIR/bin"
IMAGE_DIR="$RUN_DIR/images"
CONFIG_DIR="$RUN_DIR/config"
LOG_DIR="$RUN_DIR/logs"
PKI_DIR="$RUN_DIR/pki"
STATE_DIR="$RUN_DIR/state"
ROOTFS_MOUNT="$RUN_DIR/rootfs-mount"
GUEST_ROOTFS="$IMAGE_DIR/e2e-rootfs.ext4"
KERNEL="$IMAGE_DIR/vmlinux"
FIRECRACKER="$BIN_DIR/firecracker"

CONTROLPLANE_PID=""
AGENT_PIDS=()
KEEP_LAB="${FIREWORK_E2E_KEEP:-0}"
SCENARIO_STATUS="failed"

mkdir -p "$RUN_DIR" "$BIN_DIR" "$IMAGE_DIR" "$CONFIG_DIR" "$LOG_DIR" \
  "$PKI_DIR" "$STATE_DIR" "$ROOTFS_MOUNT"
chmod 755 "$RUN_DIR" "$LOG_DIR"

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

case "$(uname -m)" in
  x86_64) BIN_ARCH=amd64 ;;
  aarch64|arm64) BIN_ARCH=arm64 ;;
  *) die "unsupported Linux architecture in lab: $(uname -m)" ;;
esac

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command in lab: $1"
}

install_guest_tools() {
  local missing=0
  for command_name in aws curl git ip iptables jq openssl; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
      missing=1
    fi
  done
  if (( missing == 0 )); then
    return
  fi
  command -v apt-get >/dev/null 2>&1 || die "lab tools are missing and apt-get is unavailable"
  log "installing missing Linux lab tools"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq awscli curl e2fsprogs git iproute2 iptables jq openssl
}

write_file() {
  local path="$1"
  mkdir -p "$(dirname "$path")"
  cat > "$path"
}

cleanup_rootfs_mount() {
  if mountpoint -q "$ROOTFS_MOUNT"; then
    sync || true
    umount "$ROOTFS_MOUNT" || true
  fi
}

prepare_rootfs() {
  local source_rootfs="${FIREWORK_E2E_ROOTFS:-}"
  local source_kernel="${FIREWORK_E2E_KERNEL:-}"
  local source_firecracker="${FIREWORK_E2E_FIRECRACKER_BIN:-}"
  [[ -r "$source_rootfs" ]] || die "rootfs is not readable in the lab: $source_rootfs"
  [[ -r "$source_kernel" ]] || die "kernel is not readable in the lab: $source_kernel"
  [[ -x "$source_firecracker" ]] || die "Firecracker is not executable in the lab: $source_firecracker"

  cp "$source_rootfs" "$GUEST_ROOTFS"
  cp "$source_kernel" "$KERNEL"
  cp "$source_firecracker" "$FIRECRACKER"
  chmod 755 "$FIRECRACKER"

  mount -o loop "$GUEST_ROOTFS" "$ROOTFS_MOUNT"
  local busybox=""
  for candidate in /bin/busybox /sbin/busybox /usr/bin/busybox; do
    if [[ -x "$ROOTFS_MOUNT$candidate" ]]; then
      busybox="$candidate"
      break
    fi
  done
  [[ -n "$busybox" ]] || die "rootfs must contain a static BusyBox binary"
  [[ -x "$ROOTFS_MOUNT/bin/sh" || -x "$ROOTFS_MOUNT/bin/busybox" ]] || die "rootfs must provide /bin/sh"

  install -D -m 0755 "${BIN_DIR}/fc-init-linux-$BIN_ARCH" "$ROOTFS_MOUNT/sbin/fc-init"
  rm -f "$ROOTFS_MOUNT/sbin/init"
  write_file "$ROOTFS_MOUNT/sbin/init" <<'EOF'
#!/bin/sh
set -eu
busybox=/bin/busybox
if [ ! -x "$busybox" ]; then busybox=/sbin/busybox; fi
if [ ! -x "$busybox" ]; then busybox=/usr/bin/busybox; fi
role="${FIREWORK_ROLE:-responder}"
case "$role" in
  responder) port=8080 ;;
  caller)
    target="${RESPONDER_URL:-}"
    [ -n "$target" ] || exit 20
    ready=0
    attempt=0
    while [ "$attempt" -lt 30 ]; do
      if "$busybox" wget -q -O - "$target/health" >/dev/null 2>&1; then
        ready=1
        break
      fi
      attempt=$((attempt + 1))
      sleep 1
    done
    [ "$ready" -eq 1 ] || exit 21
    echo cross-node-ok > /tmp/cross-node-result
    port=8081
    ;;
  *) exit 22 ;;
esac
exec "$busybox" httpd -f -p "$port" -h /www
EOF
  chmod 755 "$ROOTFS_MOUNT/sbin/init"
  mkdir -p "$ROOTFS_MOUNT/www"
  printf 'ok\n' > "$ROOTFS_MOUNT/www/health"
  cleanup_rootfs_mount
}

setup_git_repo() {
  mkdir -p "$CONFIG_DIR/repo/services"
  write_file "$CONFIG_DIR/repo/defaults.yaml" <<EOF
kernel: "$KERNEL"
vcpus: 1
memory_mb: 128
kernel_args: "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/fc-init"
EOF
  write_file "$CONFIG_DIR/repo/services/caller.yaml" <<EOF
name: "caller"
image: "$GUEST_ROOTFS"
kernel: "$KERNEL"
node_type: "general"
network: true
port_forwards:
  - host_port: 18081
    vm_port: 8081
health_check:
  type: "http"
  port: 8081
  path: "/health"
  interval: "2s"
  timeout: "1s"
  retries: 5
env:
  FIREWORK_ROLE: "caller"
cross_node_links:
  - service: "responder"
    env: "RESPONDER_URL"
    host_port: 18080
    protocol: "http"
anti_affinity_group: "local-e2e"
EOF
  write_file "$CONFIG_DIR/repo/services/responder.yaml" <<EOF
name: "responder"
image: "$GUEST_ROOTFS"
kernel: "$KERNEL"
node_type: "general"
network: true
port_forwards:
  - host_port: 18080
    vm_port: 8080
health_check:
  type: "http"
  port: 8080
  path: "/health"
  interval: "2s"
  timeout: "1s"
  retries: 5
env:
  FIREWORK_ROLE: "responder"
anti_affinity_group: "local-e2e"
EOF
  git -C "$CONFIG_DIR/repo" init -b main >/dev/null 2>&1 || {
    git -C "$CONFIG_DIR/repo" init >/dev/null
    git -C "$CONFIG_DIR/repo" checkout -b main >/dev/null
  }
  git -C "$CONFIG_DIR/repo" config user.name firework-local-e2e
  git -C "$CONFIG_DIR/repo" config user.email firework-local-e2e@localhost
  git -C "$CONFIG_DIR/repo" add .
  git -C "$CONFIG_DIR/repo" commit -m "local e2e workload" >/dev/null
}

setup_network() {
  local namespace ip_address uplink
  ip link add name fw-e2e-br type bridge
  ip addr add 10.254.0.1/24 dev fw-e2e-br
  ip link set fw-e2e-br up
  sysctl -w net.ipv4.ip_forward=1 >/dev/null
  for node in a b; do
    namespace="fw-e2e-node-${node}"
    if [[ "$node" == a ]]; then ip_address=10.254.0.11; else ip_address=10.254.0.12; fi
    uplink="fw-e2e-${node}-up"
    ip netns add "$namespace"
    ip link add "fw-e2e-${node}" type veth peer name "$uplink"
    ip link set "fw-e2e-${node}" master fw-e2e-br
    ip link set "fw-e2e-${node}" up
    ip link set "$uplink" netns "$namespace"
    ip -n "$namespace" link set lo up
    ip -n "$namespace" link set "$uplink" up
    ip -n "$namespace" addr add "$ip_address/24" dev "$uplink"
    ip -n "$namespace" route add default via 10.254.0.1
    ip netns exec "$namespace" sysctl -w net.ipv4.ip_forward=1 >/dev/null
    ip netns exec "$namespace" sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
    ip netns exec "$namespace" sysctl -w net.ipv4.conf.default.rp_filter=0 >/dev/null
  done
  ip route add 172.16.1.0/24 via 10.254.0.11 dev fw-e2e-br
  ip route add 172.16.2.0/24 via 10.254.0.12 dev fw-e2e-br
}

write_controlplane_config() {
  local operator_token="$1"
  write_file "$CONFIG_DIR/controlplane.yaml" <<EOF
role: "all"
registry_listen_addr: "10.254.0.1:9443"
events_listen_addr: "10.254.0.1:9444"
api_listen_addr: "127.0.0.1:9445"
operator_token: "$operator_token"
ingress_domain: "local.e2e"
state:
  backend: "s3"
  prefix: "cp/v1"
  s3:
    bucket: "$E2E_BUCKET"
    region: "$AWS_REGION_VALUE"
    force_path_style: false
leader_lease_ttl: "6s"
leader_renew_interval: "2s"
node_stale_ttl: "15s"
controller_tick: "2s"
target_branch: "main"
reconcile_on_start: true
git_repo_url: "file://$CONFIG_DIR/repo"
github_webhook_secret: "local-e2e-webhook-secret"
tls:
  cert_file: "$PKI_DIR/controlplane.crt"
  key_file: "$PKI_DIR/controlplane.key"
  client_ca_file: "$PKI_DIR/ca.crt"
enrollment:
  ca_file: "$PKI_DIR/ca.crt"
  ca_key_file: "$PKI_DIR/ca.key"
  node_cert_ttl: "2h"
  bootstrap_tokens:
    - token: "local-e2e-node-a-$FIREWORK_COMMIT"
      node_id: "node-a"
    - token: "local-e2e-node-b-$FIREWORK_COMMIT"
      node_id: "node-b"
EOF
}

write_agent_config() {
  local node="$1" namespace_ip="$2" vm_subnet="$3" vm_gateway="$4" token="$5"
  local api_port uplink
  if [[ "$node" == node-a ]]; then
    api_port=18081
    uplink=fw-e2e-a-up
  else
    api_port=18082
    uplink=fw-e2e-b-up
  fi
  write_file "$CONFIG_DIR/agent-${node}.yaml" <<EOF
node_name: "$node"
node_id: "$node"
store_type: "s3"
s3_bucket: "$E2E_BUCKET"
s3_prefix: "cp/v1/"
s3_region: "$AWS_REGION_VALUE"
poll_interval: "2s"
firecracker_bin: "$FIRECRACKER"
state_dir: "$STATE_DIR/$node"
images_dir: "$IMAGE_DIR"
log_level: "debug"
api_listen_addr: "$namespace_ip:$api_port"
enable_health_checks: true
enable_network_setup: true
enable_capacity_check: false
vm_subnet: "$vm_subnet"
vm_gateway: "$vm_gateway"
vm_bridge: "br-fw-$node"
out_interface: "$uplink"
registry_url: "https://10.254.0.1:9443"
registry_server_name: "controlplane.local"
registry_cert_file: "$STATE_DIR/$node/node.crt"
registry_key_file: "$STATE_DIR/$node/node.key"
registry_ca_file: "$PKI_DIR/ca.crt"
registry_bootstrap_token: "$token"
registry_heartbeat_interval: "2s"
EOF
}

wait_http() {
  local url="$1" ca_file="$2" auth_header="${3:-}" deadline=$((SECONDS + ${FIREWORK_E2E_TIMEOUT:-600}))
  while (( SECONDS < deadline )); do
    if [[ -n "$auth_header" ]]; then
      if curl --silent --show-error --fail --cacert "$ca_file" -H "$auth_header" "$url" >/dev/null 2>&1; then return 0; fi
    elif [[ -n "$ca_file" ]]; then
      if curl --silent --show-error --fail --cacert "$ca_file" "$url" >/dev/null 2>&1; then return 0; fi
    elif curl --silent --show-error --fail "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

stop_processes() {
  local pid
  for pid in "${AGENT_PIDS[@]}" "$CONTROLPLANE_PID"; do
    [[ -n "$pid" ]] || continue
    kill -TERM "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${AGENT_PIDS[@]}" "$CONTROLPLANE_PID"; do
    [[ -n "$pid" ]] || continue
    wait "$pid" >/dev/null 2>&1 || true
  done
  AGENT_PIDS=()
  CONTROLPLANE_PID=""
}

delete_network() {
  ip route del 172.16.1.0/24 via 10.254.0.11 dev fw-e2e-br >/dev/null 2>&1 || true
  ip route del 172.16.2.0/24 via 10.254.0.12 dev fw-e2e-br >/dev/null 2>&1 || true
  ip netns del fw-e2e-node-a >/dev/null 2>&1 || true
  ip netns del fw-e2e-node-b >/dev/null 2>&1 || true
  ip link del fw-e2e-br >/dev/null 2>&1 || true
}

write_manifest() {
  local status="$1"
  local agent_pids_json
  agent_pids_json="$(printf '%s\n' "${AGENT_PIDS[@]}" | jq -Rsc 'split("\n") | map(select(length > 0) | tonumber)')"
  jq -n \
    --arg status "$status" \
    --arg mode "$RUN_MODE" \
    --arg commit "$FIREWORK_COMMIT" \
    --arg bucket "$E2E_BUCKET" \
    --arg region "$AWS_REGION_VALUE" \
    --arg run_dir "$RUN_DIR" \
    --arg controlplane_pid "${CONTROLPLANE_PID:-}" \
    --argjson agent_pids "$agent_pids_json" \
    '{status:$status,mode:$mode,firework_commit:$commit,bucket:$bucket,region:$region,run_dir:$run_dir,controlplane_pid:$controlplane_pid,agent_pids:$agent_pids}' \
    > "$RUN_DIR/manifest.json"
}

cleanup() {
  local status=$?
  set +e
  export CONTROLPLANE_CURL_URL="https://127.0.0.1:9445"
  export CONTROLPLANE_CA_FILE="$PKI_DIR/ca.crt"
  export CONTROLPLANE_OPERATOR_TOKEN="${OPERATOR_TOKEN:-}"
  export E2E_BUCKET
  export AGENT_ENDPOINTS="node-a=http://10.254.0.11:18081 node-b=http://10.254.0.12:18082"
  "$SCRIPT_DIR/collect-diagnostics.sh" "$RUN_DIR" || true
  if [[ "$status" -eq 0 ]]; then SCENARIO_STATUS="passed"; fi
  cleanup_rootfs_mount
  write_manifest "$SCENARIO_STATUS"
  if [[ "$KEEP_LAB" != "1" ]]; then
    stop_processes
    delete_network
  else
    log "retaining local E2E lab at $RUN_DIR"
    log "cleanup with: make validate-e2e-local-clean FIREWORK_E2E_MANIFEST=$RUN_DIR/manifest.json"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

install_guest_tools
for command_name in mountpoint mount umount install; do require_cmd "$command_name"; done
[[ "$(id -u)" -eq 0 ]] || die "the Linux lab runner must execute as root"
[[ -r /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm is not readable and writable in the lab"
[[ -e /dev/net/tun ]] || die "/dev/net/tun is required in the lab"

log "preparing Firecracker assets"
prepare_rootfs
"$SCRIPT_DIR/generate-pki.sh" "$PKI_DIR"
setup_git_repo
setup_network

OPERATOR_TOKEN="local-e2e-operator-$FIREWORK_COMMIT"
write_controlplane_config "$OPERATOR_TOKEN"
write_agent_config node-a 10.254.0.11 172.16.1.0/24 172.16.1.1 "local-e2e-node-a-$FIREWORK_COMMIT"
write_agent_config node-b 10.254.0.12 172.16.2.0/24 172.16.2.1 "local-e2e-node-b-$FIREWORK_COMMIT"

log "starting combined control plane"
"$BIN_DIR/firework-controlplane-linux-$BIN_ARCH" --config "$CONFIG_DIR/controlplane.yaml" > "$LOG_DIR/controlplane.log" 2>&1 &
CONTROLPLANE_PID=$!
wait_http "https://127.0.0.1:9445/healthz" "$PKI_DIR/ca.crt" || die "control-plane API did not become healthy"

log "starting two isolated agents"
ip netns exec fw-e2e-node-a "$BIN_DIR/firework-agent-linux-$BIN_ARCH" --config "$CONFIG_DIR/agent-node-a.yaml" > "$LOG_DIR/node-a.log" 2>&1 &
AGENT_PIDS+=("$!")
ip netns exec fw-e2e-node-b "$BIN_DIR/firework-agent-linux-$BIN_ARCH" --config "$CONFIG_DIR/agent-node-b.yaml" > "$LOG_DIR/node-b.log" 2>&1 &
AGENT_PIDS+=("$!")
wait_http "http://10.254.0.11:18081/healthz" "" || die "node-a API did not become healthy"
wait_http "http://10.254.0.12:18082/healthz" "" || die "node-b API did not become healthy"

nodes_url="https://127.0.0.1:9445/v1/nodes"
services_url="https://127.0.0.1:9445/v1/services"
auth_header="Authorization: Bearer $OPERATOR_TOKEN"
deadline=$((SECONDS + ${FIREWORK_E2E_TIMEOUT:-600}))
nodes_json=""
services_json=""
while (( SECONDS < deadline )); do
  nodes_json="$(curl --silent --show-error --fail --cacert "$PKI_DIR/ca.crt" -H "$auth_header" "$nodes_url" 2>/dev/null || true)"
  services_json="$(curl --silent --show-error --fail --cacert "$PKI_DIR/ca.crt" -H "$auth_header" "$services_url" 2>/dev/null || true)"
  if jq -e '.count == 2 and ([.items[].node_id] | unique | length == 2) and all(.items[]; .state == "ready")' <<<"$nodes_json" >/dev/null 2>&1 \
    && jq -e '.count == 2 and all(.items[]; .state == "running" and .health == "healthy") and ([.items[].node] | unique | length == 2)' <<<"$services_json" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

jq -e '.count == 2 and ([.items[].node_id] | unique | length == 2)' <<<"$nodes_json" >/dev/null \
  || die "two nodes did not register: $nodes_json"
jq -e '.count == 2 and all(.items[]; .state == "running" and .health == "healthy") and ([.items[].node] | unique | length == 2)' <<<"$services_json" >/dev/null \
  || die "two healthy cross-node services did not converge: $services_json"

caller_node="$(jq -r '.items[] | select(.name == "caller") | .node' <<<"$services_json")"
case "$caller_node" in
  node-a) caller_endpoint="http://10.254.0.11:18081" ;;
  node-b) caller_endpoint="http://10.254.0.12:18082" ;;
  *) die "caller service has no valid node placement: $caller_node" ;;
esac
curl --silent --show-error --fail "$caller_endpoint/health" >/dev/null \
  || die "caller endpoint did not become reachable; cross-node link likely failed"

for node in node-a node-b; do
  aws s3api head-object --bucket "$E2E_BUCKET" --key "cp/v1/nodes/$node.yaml" >/dev/null \
    || die "real S3 bucket has no rendered config for $node"
done
aws s3api list-objects-v2 --bucket "$E2E_BUCKET" --prefix cp/v1/ --output json > "$RUN_DIR/s3-inventory.json"
printf '%s\n' "$nodes_json" > "$RUN_DIR/nodes.json"
printf '%s\n' "$services_json" > "$RUN_DIR/services.json"
SCENARIO_STATUS="passed"
log "local two-node E2E passed"
log "node placement: $(jq -r '.items[] | [.name, .node] | @tsv' <<<"$services_json" | tr '\n' ' ')"
