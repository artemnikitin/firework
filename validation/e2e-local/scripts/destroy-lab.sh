#!/usr/bin/env bash
set -Eeuo pipefail

manifest=${1:-}
if [[ -z "$manifest" ]]; then
  printf 'usage: make validate-e2e-local-clean FIREWORK_E2E_MANIFEST=/path/to/manifest.json\n' >&2
  exit 2
fi

command -v jq >/dev/null 2>&1 || { printf 'ERROR: jq is required\n' >&2; exit 1; }
command -v aws >/dev/null 2>&1 || { printf 'ERROR: aws is required\n' >&2; exit 1; }

bucket=$(jq -r '.bucket // empty' "$manifest")
region=$(jq -r '.region // empty' "$manifest")
mode=$(jq -r '.mode // empty' "$manifest")
instance=$(jq -r '.lima_instance // empty' "$manifest")

[[ -n "$bucket" && -n "$region" ]] || { printf 'ERROR: invalid manifest: %s\n' "$manifest" >&2; exit 1; }
export AWS_REGION="$region"
export AWS_DEFAULT_REGION="$region"
export AWS_EC2_METADATA_DISABLED=true

if [[ "$mode" == lima && -n "$instance" ]] && command -v limactl >/dev/null 2>&1; then
  limactl stop --force "$instance" >/dev/null 2>&1 || true
  limactl delete --force "$instance" >/dev/null 2>&1 || true
elif [[ "$mode" == linux ]]; then
  for pid in $(jq -r '(.agent_pids[]?, .controlplane_pid?) | select(type == "number")' "$manifest"); do
    sudo kill -TERM "$pid" >/dev/null 2>&1 || true
  done
  for namespace in fw-e2e-node-a fw-e2e-node-b; do
    sudo ip netns del "$namespace" >/dev/null 2>&1 || true
  done
  sudo ip link del fw-e2e-br >/dev/null 2>&1 || true
fi

aws s3 rm "s3://$bucket" --recursive >/dev/null
aws s3api delete-bucket --bucket "$bucket" >/dev/null
printf 'destroyed local E2E lab from %s\n' "$manifest"
