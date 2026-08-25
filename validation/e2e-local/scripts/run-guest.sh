#!/usr/bin/env bash
set -Eeuo pipefail

RUN_DIR=${1:?usage: run-guest.sh RUN_DIR BUCKET REGION COMMIT MODE IMAGES_BUCKET}
E2E_BUCKET=${2:?missing bucket}
AWS_REGION_VALUE=${3:?missing AWS region}
FIREWORK_COMMIT=${4:?missing Firework commit}
RUN_MODE=${5:?missing run mode}
IMAGES_BUCKET=${6:?missing workload image bucket}

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
DIRECT_BIN_DIR="$RUN_DIR/direct-bin"
KERNEL="$IMAGE_DIR/vmlinux"
FIRECRACKER="$BIN_DIR/firecracker"
ASSET_SCRIPT="${FIREWORK_E2E_ASSET_SCRIPT:-$SCRIPT_DIR/prepare-assets.sh}"
ES_IMAGE_KEY="${FIREWORK_E2E_ES_IMAGE_KEY:-tenant-2-elasticsearch-rootfs.ext4}"
KIBANA_IMAGE_KEY="${FIREWORK_E2E_KIBANA_IMAGE_KEY:-tenant-2-kibana-rootfs.ext4}"
VOLUME_SIZE="${FIREWORK_E2E_VOLUME_SIZE:-2Gi}"
STORAGE_CAPACITY="${FIREWORK_E2E_STORAGE_CAPACITY:-8Gi}"
HEALTH_RETRIES="${FIREWORK_E2E_HEALTH_RETRIES:-80}"
E2E_TIMEOUT="${FIREWORK_E2E_TIMEOUT:-1800}"
ES_VCPUS="${FIREWORK_E2E_ES_VCPUS:-${FIREWORK_E2E_SERVICE_VCPUS:-4}}"
ES_MEMORY_MB="${FIREWORK_E2E_ES_MEMORY_MB:-${FIREWORK_E2E_SERVICE_MEMORY_MB:-6144}}"
KIBANA_VCPUS="${FIREWORK_E2E_KIBANA_VCPUS:-${FIREWORK_E2E_SERVICE_VCPUS:-2}}"
KIBANA_MEMORY_MB="${FIREWORK_E2E_KIBANA_MEMORY_MB:-${FIREWORK_E2E_SERVICE_MEMORY_MB:-4096}}"
# Keep the boot argument explicit for guests that expose a usable arm64 CPU
# random source. The Firecracker VM config also enables VirtIO-RNG for VZ/KVM
# environments where that source is not available.
KERNEL_BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on"
# Existing GitOps images bundle an fc-init that predates firework.env64, so
# keep the default override to one whitespace-free JVM option. A rebuilt image
# with the current fc-init can override this with multiple options.
ES_JAVA_OPTS="${FIREWORK_E2E_ES_JAVA_OPTS--Xmx1g}"
WEBHOOK_SECRET="local-e2e-webhook-secret"
ROOT_OUT_INTERFACE=""

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
  for command_name in aws curl git ip iptables jq openssl sha256sum tar; do
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
  apt-get install -y -qq awscli ca-certificates curl e2fsprogs file git iproute2 iptables jq openssl tar
}

prepare_direct_launcher_path() {
  local command_name command_path
  mkdir -p "$DIRECT_BIN_DIR"
  # Lima's Ubuntu guest has systemd, so the agent would otherwise select
  # systemd-run. Its transient-unit PID exposes systemd-executor through
  # /proc/<pid>/exe, which cannot satisfy Firecracker ownership checks. Keep
  # the agent's PATH deliberately small so chooseLauncher selects the direct
  # process launcher while retaining the host tools used by networking and
  # local-volume management.
  for command_name in \
    e2fsck ip iptables mount mountpoint mkfs.ext4 resize2fs sh sysctl \
    tune2fs umount; do
    command_path="$(command -v "$command_name" || true)"
    [[ -n "$command_path" ]] || die "missing direct-launcher dependency: $command_name"
    ln -sf "$command_path" "$DIRECT_BIN_DIR/$command_name"
  done
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

prepare_assets() {
  [[ -x "$ASSET_SCRIPT" ]] || die "asset preparation script is missing: $ASSET_SCRIPT"
  FIREWORK_E2E_FIRECRACKER_BIN="$FIRECRACKER" FIREWORK_E2E_KERNEL="$KERNEL" \
    FIREWORK_E2E_FIRECRACKER_VERSION="${FIREWORK_E2E_FIRECRACKER_VERSION:-1.12.0}" \
    FIREWORK_E2E_KERNEL_KEY="${FIREWORK_E2E_KERNEL_KEY:-firecracker-ci/v1.12/aarch64/vmlinux-5.10.233}" \
    bash "$ASSET_SCRIPT" "$RUN_DIR"
  [[ -x "$FIRECRACKER" ]] || die "prepared Firecracker binary is missing"
  [[ -r "$KERNEL" ]] || die "prepared kernel is missing"
}

setup_git_repo() {
  mkdir -p "$CONFIG_DIR/repo/services"
  write_file "$CONFIG_DIR/repo/defaults.yaml" <<EOF
kernel: "$KERNEL"
vcpus: $ES_VCPUS
memory_mb: $ES_MEMORY_MB
kernel_args: "$KERNEL_BOOT_ARGS init=/sbin/fc-init"
EOF
  write_stateful_services
  git -C "$CONFIG_DIR/repo" init -b main >/dev/null 2>&1 || {
    git -C "$CONFIG_DIR/repo" init >/dev/null
    git -C "$CONFIG_DIR/repo" checkout -b main >/dev/null
  }
  git -C "$CONFIG_DIR/repo" config user.name firework-local-e2e
  git -C "$CONFIG_DIR/repo" config user.email firework-local-e2e@localhost
  git -C "$CONFIG_DIR/repo" add .
  git -C "$CONFIG_DIR/repo" commit -m "local e2e stateful workload" >/dev/null
}

write_stateful_services() {
  local anti_affinity="${1:-}"
  local volume=""
  if [[ "${FIREWORK_E2E_ENABLE_VOLUME:-1}" == 1 ]]; then
    volume="$(printf 'volumes:\n  - name: data\n    type: local\n    mount_path: /usr/share/elasticsearch/data\n    size: %s\n' "$VOLUME_SIZE")"
  fi
  write_file "$CONFIG_DIR/repo/services/elasticsearch.yaml" <<EOF
name: "tenant-2-elasticsearch"
image: "$IMAGE_DIR/$ES_IMAGE_KEY"
kernel: "$KERNEL"
node_type: "stateful"
vcpus: $ES_VCPUS
memory_mb: $ES_MEMORY_MB
kernel_args: "$KERNEL_BOOT_ARGS init=/sbin/fc-init /bin/tini -- /usr/local/bin/docker-entrypoint.sh eswrapper"
network: true
port_forwards:
  - host_port: 19200
    vm_port: 9200
health_check:
  type: "http"
  port: 9200
  path: "/_cluster/health"
  interval: "15s"
  timeout: "10s"
  retries: $HEALTH_RETRIES
env:
  TENANT_ID: "local-stateful"
  ES_JAVA_OPTS: "$ES_JAVA_OPTS"
$volume$anti_affinity
EOF
  write_file "$CONFIG_DIR/repo/services/kibana.yaml" <<EOF
name: "tenant-2-kibana"
image: "$IMAGE_DIR/$KIBANA_IMAGE_KEY"
kernel: "$KERNEL"
node_type: "stateful"
vcpus: $KIBANA_VCPUS
memory_mb: $KIBANA_MEMORY_MB
kernel_args: "$KERNEL_BOOT_ARGS init=/sbin/fc-init /bin/tini -- /usr/local/bin/kibana-docker"
network: true
port_forwards:
  - host_port: 15612
    vm_port: 5601
health_check:
  type: "http"
  port: 5601
  path: "/api/status"
  interval: "30s"
  timeout: "10s"
  retries: $HEALTH_RETRIES
env:
  TENANT_ID: "local-stateful"
cross_node_links:
  - service: "tenant-2-elasticsearch"
    env: "ELASTICSEARCH_HOSTS"
    host_port: 19200
    protocol: "http"
$anti_affinity
EOF
}

commit_stateful_update() {
  local message="$1"
  git -C "$CONFIG_DIR/repo" add services
  git -C "$CONFIG_DIR/repo" commit -m "$message" >/dev/null
}

setup_network() {
  local namespace ip_address uplink resolver_source
  resolver_source="/run/systemd/resolve/resolv.conf"
  if [[ ! -r "$resolver_source" ]]; then
    resolver_source="$RUN_DIR/resolv.conf"
    cp /etc/resolv.conf "$resolver_source"
    sed -i 's/127\.0\.0\.53/192.168.5.2/g' "$resolver_source"
  fi
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
    mkdir -p "/etc/netns/$namespace"
    cp "$resolver_source" "/etc/netns/$namespace/resolv.conf"
    ip netns exec "$namespace" sysctl -w net.ipv4.ip_forward=1 >/dev/null
    ip netns exec "$namespace" sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
    ip netns exec "$namespace" sysctl -w net.ipv4.conf.default.rp_filter=0 >/dev/null
  done
  mkdir -p "$RUN_DIR/storage/node-a" "$RUN_DIR/storage/node-b"
  storage_mount_size="${STORAGE_CAPACITY//i/}"
  mount -t tmpfs -o "size=$storage_mount_size" firework-e2e-storage-a "$RUN_DIR/storage/node-a"
  mount -t tmpfs -o "size=$storage_mount_size" firework-e2e-storage-b "$RUN_DIR/storage/node-b"
  ROOT_OUT_INTERFACE="$(ip -o route show default | awk 'NR == 1 {print $5}')"
  [[ -n "$ROOT_OUT_INTERFACE" ]] || die "could not determine the Lima guest default interface"
  iptables -t nat -A POSTROUTING -s 10.254.0.0/24 -o "$ROOT_OUT_INTERFACE" -j MASQUERADE
  iptables -A FORWARD -s 10.254.0.0/24 -j ACCEPT
  iptables -A FORWARD -d 10.254.0.0/24 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
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
s3_images_bucket: "$IMAGES_BUCKET"
poll_interval: "2s"
firecracker_bin: "$FIRECRACKER"
state_dir: "$STATE_DIR/$node"
images_dir: "$IMAGE_DIR"
log_level: "debug"
api_listen_addr: "$namespace_ip:$api_port"
enable_health_checks: true
enable_network_setup: true
enable_capacity_check: true
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
storage:
  local:
    path: "$RUN_DIR/storage/$node"
    capacity: "$STORAGE_CAPACITY"
EOF
}

wait_http() {
  local url="$1" ca_file="$2" auth_header="${3:-}" deadline=$((SECONDS + E2E_TIMEOUT))
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

api_get() {
  local path="$1"
  curl --silent --show-error --fail --cacert "$PKI_DIR/ca.crt" \
    -H "Authorization: Bearer $OPERATOR_TOKEN" "https://127.0.0.1:9445$path"
}

wait_nodes() {
  local count="$1" deadline=$((SECONDS + E2E_TIMEOUT)) nodes
  while (( SECONDS < deadline )); do
    nodes="$(api_get /v1/nodes 2>/dev/null || true)"
    if jq -e --argjson count "$count" '.count == $count and all(.items[]; .state == "ready")' <<<"$nodes" >/dev/null 2>&1; then
      printf '%s\n' "$nodes"
      return 0
    fi
    sleep 2
  done
  printf '%s\n' "${nodes:-{}}" > "$RUN_DIR/nodes-timeout.json"
  return 1
}

wait_services_healthy() {
  local expected="$1" distinct_nodes="$2" deadline=$((SECONDS + E2E_TIMEOUT)) services
  while (( SECONDS < deadline )); do
    services="$(api_get /v1/services 2>/dev/null || true)"
    if jq -e --argjson expected "$expected" --argjson distinct "$distinct_nodes" \
      '.count == $expected and all(.items[]; .state == "running" and .health == "healthy") and (([.items[].node] | unique | length) == $distinct)' \
      <<<"$services" >/dev/null 2>&1; then
      printf '%s\n' "$services"
      return 0
    fi
    sleep 2
  done
  printf '%s\n' "${services:-{}}" > "$RUN_DIR/services-timeout.json"
  return 1
}

wait_empty_state() {
  local deadline=$((SECONDS + E2E_TIMEOUT)) services status
  while (( SECONDS < deadline )); do
    services="$(api_get /v1/services 2>/dev/null || true)"
    status="$(curl --silent --show-error --fail http://10.254.0.11:18081/status 2>/dev/null || true)"
    if jq -e '.count == 0' <<<"$services" >/dev/null 2>&1 && \
      jq -e '.desired_services == 0 and (.services | length == 0)' <<<"$status" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

service_node() {
  local name="$1" services
  services="$(api_get /v1/services)"
  jq -r --arg name "$name" '.items[] | select(.name == $name) | .node' <<<"$services"
}

node_host() {
  case "$1" in
    node-a) printf '10.254.0.11\n' ;;
    node-b) printf '10.254.0.12\n' ;;
    *) return 1 ;;
  esac
}

service_url() {
  local name="$1" port="$2" node
  node="$(service_node "$name")"
  printf 'http://%s:%s\n' "$(node_host "$node")" "$port"
}

wait_service_http() {
  local url="$1" deadline=$((SECONDS + E2E_TIMEOUT))
  while (( SECONDS < deadline )); do
    if curl --silent --show-error --fail "$url" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  return 1
}

assert_elasticsearch_green() {
  local endpoint="$1" response
  wait_service_http "$endpoint/_cluster/health" || die "Elasticsearch endpoint did not become reachable: $endpoint"
  curl --silent --show-error --fail -X PUT "$endpoint/_settings" \
    -H 'Content-Type: application/json' \
    --data '{"index":{"number_of_replicas":0}}' >/dev/null
  local deadline=$((SECONDS + E2E_TIMEOUT))
  while (( SECONDS < deadline )); do
    response="$(curl --silent --show-error --fail "$endpoint/_cluster/health" 2>/dev/null || true)"
    if jq -e '.status == "green"' <<<"$response" >/dev/null 2>&1; then
      printf '%s\n' "$response" > "$RUN_DIR/elasticsearch-health.json"
      return 0
    fi
    sleep 5
  done
  printf '%s\n' "${response:-{}}" > "$RUN_DIR/elasticsearch-health-timeout.json"
  return 1
}

send_webhook() {
  local delivery_id="$1" payload signature
  payload="$(jq -cn --arg ref refs/heads/main --arg url "file://$CONFIG_DIR/repo" \
    '{ref:$ref,repository:{clone_url:$url}}')"
  signature="$(printf '%s' "$payload" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -binary | od -An -vtx1 | tr -d ' \n')"
  curl --silent --show-error --fail --cacert "$PKI_DIR/ca.crt" \
    --resolve controlplane.local:9444:10.254.0.1 \
    -H 'Content-Type: application/json' \
    -H 'X-GitHub-Event: push' \
    -H "X-GitHub-Delivery: $delivery_id" \
    -H "X-Hub-Signature-256: sha256=$signature" \
    --data "$payload" https://controlplane.local:9444/v1/events/github >/dev/null
}

start_agent() {
  local node="$1"
  PATH="$DIRECT_BIN_DIR" ip netns exec "fw-e2e-$node" "$BIN_DIR/firework-agent-linux-$BIN_ARCH" \
    --config "$CONFIG_DIR/agent-$node.yaml" > "$LOG_DIR/$node.log" 2>&1 &
  AGENT_PIDS+=("$!")
}

stop_agent() {
  local index="$1"
  local pid="${AGENT_PIDS[$index]:-}"
  [[ -n "$pid" ]] || return 0
  kill -TERM "$pid" >/dev/null 2>&1 || true
  wait "$pid" >/dev/null 2>&1 || true
  AGENT_PIDS[index]=""
}

commit_and_notify() {
  local message="$1" delivery="$2"
  commit_stateful_update "$message"
  send_webhook "$delivery"
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
  rm -rf /etc/netns/fw-e2e-node-a /etc/netns/fw-e2e-node-b
  umount "$RUN_DIR/storage/node-a" >/dev/null 2>&1 || true
  umount "$RUN_DIR/storage/node-b" >/dev/null 2>&1 || true
  if [[ -n "$ROOT_OUT_INTERFACE" ]]; then
    iptables -t nat -D POSTROUTING -s 10.254.0.0/24 -o "$ROOT_OUT_INTERFACE" -j MASQUERADE >/dev/null 2>&1 || true
    iptables -D FORWARD -s 10.254.0.0/24 -j ACCEPT >/dev/null 2>&1 || true
    iptables -D FORWARD -d 10.254.0.0/24 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT >/dev/null 2>&1 || true
  fi
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
    --arg images_bucket "$IMAGES_BUCKET" \
    --arg es_image "$ES_IMAGE_KEY" \
    --arg kibana_image "$KIBANA_IMAGE_KEY" \
    --arg region "$AWS_REGION_VALUE" \
    --arg run_dir "$RUN_DIR" \
    --arg controlplane_pid "${CONTROLPLANE_PID:-}" \
    --argjson agent_pids "$agent_pids_json" \
    '{status:$status,mode:$mode,scenario:"stateful",firework_commit:$commit,bucket:$bucket,images_bucket:$images_bucket,images:{elasticsearch:$es_image,kibana:$kibana_image},region:$region,run_dir:$run_dir,controlplane_pid:$controlplane_pid,agent_pids:$agent_pids}' \
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
prepare_assets
"$SCRIPT_DIR/generate-pki.sh" "$PKI_DIR"
setup_git_repo
setup_network
prepare_direct_launcher_path

OPERATOR_TOKEN="local-e2e-operator-$FIREWORK_COMMIT"
write_controlplane_config "$OPERATOR_TOKEN"
write_agent_config node-a 10.254.0.11 172.16.1.0/24 172.16.1.1 "local-e2e-node-a-$FIREWORK_COMMIT"
write_agent_config node-b 10.254.0.12 172.16.2.0/24 172.16.2.1 "local-e2e-node-b-$FIREWORK_COMMIT"

log "starting combined control plane"
"$BIN_DIR/firework-controlplane-linux-$BIN_ARCH" --config "$CONFIG_DIR/controlplane.yaml" > "$LOG_DIR/controlplane.log" 2>&1 &
CONTROLPLANE_PID=$!
wait_http "https://127.0.0.1:9445/healthz" "$PKI_DIR/ca.crt" || die "control-plane API did not become healthy"

log "starting node-a and proving the colocated stateful topology"
start_agent node-a
wait_http "http://10.254.0.11:18081/healthz" "" || die "node-a API did not become healthy"
nodes_json="$(wait_nodes 1)" || die "node-a did not become ready"
services_json="$(wait_services_healthy 2 1)" || die "stateful services did not converge in the colocated topology"
printf '%s\n' "$nodes_json" > "$RUN_DIR/nodes-colocated.json"
printf '%s\n' "$services_json" > "$RUN_DIR/services-colocated.json"
es_endpoint="$(service_url tenant-2-elasticsearch 19200)"
kibana_endpoint="$(service_url tenant-2-kibana 15612)"
assert_elasticsearch_green "$es_endpoint" || die "Elasticsearch did not become green in the colocated topology"
wait_service_http "$kibana_endpoint/api/status" || die "Kibana did not become reachable in the colocated topology"
[[ -s "$IMAGE_DIR/$ES_IMAGE_KEY" && -s "$IMAGE_DIR/$KIBANA_IMAGE_KEY" ]] \
  || die "the agent did not download both real S3 workload images"

log "starting node-b and checking that the colocated assignment remains stable"
start_agent node-b
wait_http "http://10.254.0.12:18082/healthz" "" || die "node-b API did not become healthy"
nodes_json="$(wait_nodes 2)" || die "two nodes did not become ready"
services_json="$(wait_services_healthy 2 1)" || die "services unexpectedly failed while node-b enrolled"
printf '%s\n' "$nodes_json" > "$RUN_DIR/nodes-two-ready.json"
printf '%s\n' "$services_json" > "$RUN_DIR/services-colocated-two-nodes.json"

log "updating the same stateful workload with anti-affinity and proving movement"
write_stateful_services 'anti_affinity_group: "stateful"'
commit_and_notify "split stateful services across nodes" "split-$FIREWORK_COMMIT"
services_json="$(wait_services_healthy 2 2)" || die "stateful services did not converge after movement"
printf '%s\n' "$services_json" > "$RUN_DIR/services-split.json"
es_endpoint="$(service_url tenant-2-elasticsearch 19200)"
kibana_endpoint="$(service_url tenant-2-kibana 15612)"
assert_elasticsearch_green "$es_endpoint" || die "Elasticsearch did not become green after movement"
wait_service_http "$kibana_endpoint/api/status" || die "Kibana did not become reachable after movement"

log "restarting node-a and verifying surviving VM and local-volume adoption"
marker="local-validation-$(date -u +%Y%m%dT%H%M%SZ)"
curl --silent --show-error --fail -X PUT "$es_endpoint/local-validation/_doc/marker" \
  -H 'Content-Type: application/json' --data "{\"marker\":\"$marker\"}" >/dev/null
stop_agent 0
start_agent node-a
wait_http "http://10.254.0.11:18081/healthz" "" || die "node-a did not recover after agent restart"
services_json="$(wait_services_healthy 2 2)" || die "services did not converge after agent restart"
printf '%s\n' "$services_json" > "$RUN_DIR/services-after-agent-restart.json"
curl --silent --show-error --fail "$es_endpoint/local-validation/_doc/marker" \
  | jq -e --arg marker "$marker" '._source.marker == $marker' >/dev/null 2>&1 \
  || die "Elasticsearch volume marker was not readable after agent restart"

log "removing the desired state and checking explicit empty assignment convergence"
rm -f "$CONFIG_DIR/repo/services"/*.yaml
commit_and_notify "remove stateful workload" "empty-$FIREWORK_COMMIT"
wait_empty_state || die "agents did not converge to an explicit empty desired state"

log "restoring the stateful workload for stale-node and port ownership checks"
write_stateful_services 'anti_affinity_group: "stateful"'
commit_and_notify "restore stateful workload" "restore-$FIREWORK_COMMIT"
services_json="$(wait_services_healthy 2 2)" || die "stateful workload did not restore after empty state"

log "stopping node-b and asserting stale-node visibility before recovery"
stop_agent 1
stale_deadline=$((SECONDS + 90))
stale_seen=0
while (( SECONDS < stale_deadline )); do
  nodes_json="$(api_get /v1/nodes 2>/dev/null || true)"
  if jq -e '.items[] | select(.node_id == "node-b" and .state == "down")' <<<"$nodes_json" >/dev/null 2>&1; then
    stale_seen=1
    break
  fi
  sleep 2
done
[[ "$stale_seen" == 1 ]] || die "node-b did not become visibly down after heartbeat loss"
start_agent node-b
wait_http "http://10.254.0.12:18082/healthz" "" || die "node-b did not recover after stale-node check"
services_json="$(wait_services_healthy 2 2)" || die "services did not reconverge after stale-node recovery"

log "checking per-node host-port ownership and rendered S3 state"
es_node="$(service_node tenant-2-elasticsearch)"
kibana_node="$(service_node tenant-2-kibana)"
[[ "$es_node" != "$kibana_node" ]] || die "anti-affinity placement regressed: both services are on $es_node"
jq -e '[.items[].port_forwards[]?.host_port] as $ports | ($ports | length) == ($ports | unique | length)' <<<"$services_json" >/dev/null \
  || die "the final state contains duplicate host-port ownership"
for node in node-a node-b; do
  aws s3api head-object --bucket "$E2E_BUCKET" --key "cp/v1/nodes/$node.yaml" >/dev/null \
    || die "real S3 bucket has no rendered config for $node"
done
aws s3api list-objects-v2 --bucket "$E2E_BUCKET" --prefix cp/v1/ --output json > "$RUN_DIR/s3-inventory.json"
printf '%s\n' "$nodes_json" > "$RUN_DIR/nodes.json"
printf '%s\n' "$services_json" > "$RUN_DIR/services.json"
cp "$RUN_DIR/assets-manifest.json" "$RUN_DIR/asset-provenance.json"
SCENARIO_STATUS="passed"
log "local stateful two-node E2E passed"
log "final node placement: $(jq -r '.items[] | [.name, .node] | @tsv' <<<"$services_json" | tr '\n' ' ')"
