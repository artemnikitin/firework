#!/usr/bin/env bash
set -u

RUN_DIR=${1:?usage: collect-diagnostics.sh RUN_DIR}
mkdir -p "$RUN_DIR/diagnostics"

out="$RUN_DIR/diagnostics"

for config in "$RUN_DIR"/config/*.yaml; do
  [[ -f "$config" ]] || continue
  name=$(basename "$config")
  sed -E \
    -e 's/^([[:space:]]*(operator_token|registry_bootstrap_token|github_webhook_secret):).*/\1 "<redacted>"/' \
    -e 's/^([[:space:]]*-[[:space:]]*token:).*/\1 "<redacted>"/' \
    "$config" > "$out/$name"
done

ip address show > "$out/ip-address.txt" 2>&1 || true
ip route show table all > "$out/ip-routes.txt" 2>&1 || true
iptables-save > "$out/iptables.txt" 2>&1 || true

for namespace in fw-e2e-node-a fw-e2e-node-b; do
  ip netns exec "$namespace" ip address show > "$out/$namespace-ip-address.txt" 2>&1 || true
  ip netns exec "$namespace" ip route show table all > "$out/$namespace-ip-routes.txt" 2>&1 || true
  ip netns exec "$namespace" iptables-save > "$out/$namespace-iptables.txt" 2>&1 || true
done

if [[ -n "${CONTROLPLANE_PID:-}" ]]; then
  ps -o pid,ppid,state,etime,args -p "$CONTROLPLANE_PID" > "$out/controlplane-process.txt" 2>&1 || true
fi
for pid in ${AGENT_PIDS:-}; do
  ps -o pid,ppid,state,etime,args -p "$pid" >> "$out/agent-processes.txt" 2>&1 || true
done

if [[ -n "${CONTROLPLANE_CURL_URL:-}" && -n "${CONTROLPLANE_CA_FILE:-}" ]]; then
  curl --silent --show-error --cacert "$CONTROLPLANE_CA_FILE" \
    -H "Authorization: Bearer ${CONTROLPLANE_OPERATOR_TOKEN:-}" \
    "$CONTROLPLANE_CURL_URL/v1/nodes" > "$out/controlplane-nodes.json" 2>&1 || true
  curl --silent --show-error --cacert "$CONTROLPLANE_CA_FILE" \
    -H "Authorization: Bearer ${CONTROLPLANE_OPERATOR_TOKEN:-}" \
    "$CONTROLPLANE_CURL_URL/v1/services" > "$out/controlplane-services.json" 2>&1 || true
fi

for endpoint in ${AGENT_ENDPOINTS:-}; do
  name=${endpoint%%=*}
  url=${endpoint#*=}
  curl --silent --show-error --fail "$url/status" > "$out/${name}-status.json" 2>&1 || true
done

if [[ -n "${E2E_BUCKET:-}" ]]; then
  aws s3api list-objects-v2 --bucket "$E2E_BUCKET" --prefix cp/v1/ \
    --output json > "$out/s3-cp-v1-inventory.json" 2>&1 || true
fi
