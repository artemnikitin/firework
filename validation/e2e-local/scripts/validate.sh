#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
GUEST_RUNNER="$SCRIPT_DIR/run-guest.sh"

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require_cmd aws
require_cmd curl
require_cmd git
require_cmd go
require_cmd jq
require_cmd make

if [[ -z "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_PROFILE:-}" ]]; then
  eval "$(aws configure export-credentials --profile "$AWS_PROFILE" --format env)"
  unset AWS_PROFILE
fi

export AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-}}"
export AWS_DEFAULT_REGION="$AWS_REGION"
[[ -n "$AWS_REGION" ]] || die "AWS_REGION or AWS_DEFAULT_REGION is required"
aws sts get-caller-identity >/dev/null || die "AWS credentials are not usable"

account_id="$(aws sts get-caller-identity --query Account --output text)"
run_id="$(date -u +%Y%m%dt%H%M%S)-$$"
bucket="firework-local-e2e-${account_id}-${run_id//[^a-zA-Z0-9-]/-}"
bucket="${bucket:0:63}"

case "$(uname -s)" in
  Darwin)
    host_arch="arm64"
    default_mode="lima"
    ;;
  Linux)
    case "$(uname -m)" in
      x86_64) host_arch="amd64" ;;
      aarch64|arm64) host_arch="arm64" ;;
      *) die "unsupported Linux architecture: $(uname -m)" ;;
    esac
    default_mode="linux"
    ;;
  *) die "unsupported host OS: $(uname -s)" ;;
esac
mode="${FIREWORK_E2E_MODE:-$default_mode}"
[[ "$mode" == linux || "$mode" == lima ]] || die "FIREWORK_E2E_MODE must be linux or lima"
if [[ "$mode" == linux && "$(uname -s)" != Linux ]]; then
  die "FIREWORK_E2E_MODE=linux requires a Linux host"
fi
if [[ "$mode" == lima && "$(uname -s)" != Darwin ]]; then
  die "FIREWORK_E2E_MODE=lima requires macOS"
fi

[[ -n "${FIREWORK_E2E_FIRECRACKER_BIN:-}" && -x "$FIREWORK_E2E_FIRECRACKER_BIN" ]] || die "set executable FIREWORK_E2E_FIRECRACKER_BIN"
[[ -n "${FIREWORK_E2E_KERNEL:-}" && -r "$FIREWORK_E2E_KERNEL" ]] || die "set readable FIREWORK_E2E_KERNEL"
[[ -n "${FIREWORK_E2E_ROOTFS:-}" && -r "$FIREWORK_E2E_ROOTFS" ]] || die "set readable FIREWORK_E2E_ROOTFS"

log "building Linux Firework binaries"
(cd "$REPO_ROOT" && make "build-linux-$host_arch" >/dev/null)

commit="$(git -C "$REPO_ROOT" rev-parse HEAD)"
if [[ -n "${FIREWORK_E2E_WORKDIR:-}" ]]; then
  host_run_dir="$FIREWORK_E2E_WORKDIR"
else
  host_run_dir="$(mktemp -d "${TMPDIR:-/tmp}/firework-e2e-local.XXXXXX")"
fi
mkdir -p "$host_run_dir"

instance=""
guest_run_dir="/tmp/firework-e2e-local-$run_id"
status=1
bucket_created=0

cleanup_bucket() {
  local cleanup_status=0
  if [[ "$bucket_created" -ne 1 || "${FIREWORK_E2E_KEEP:-0}" == 1 ]]; then
    return 0
  fi
  log "deleting real S3 bucket $bucket"
  aws s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || cleanup_status=1
  aws s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || cleanup_status=1
  if [[ "$cleanup_status" -ne 0 ]]; then
    printf 'WARNING: bucket cleanup failed; inspect and delete only %s manually\n' "$bucket" >&2
  fi
  return "$cleanup_status"
}

cleanup_runtime() {
  set +e
  if [[ "$mode" == lima && -n "$instance" ]]; then
    if [[ "${FIREWORK_E2E_KEEP:-0}" == 1 ]]; then
      limactl shell --yes "$instance" -- sudo -n chmod 644 "$guest_run_dir/manifest.json" >/dev/null 2>&1 || true
      limactl copy "$instance:$guest_run_dir/manifest.json" "$host_run_dir/manifest.json" >/dev/null 2>&1 || true
    else
      limactl shell --yes "$instance" -- sudo -n chmod -R a+rX "$guest_run_dir" >/dev/null 2>&1 || true
      limactl copy --recursive "$instance:$guest_run_dir" "$host_run_dir/guest" >/dev/null 2>&1 || true
    fi
    if [[ "${FIREWORK_E2E_KEEP:-0}" == 1 && -f "$host_run_dir/manifest.json" ]]; then
      jq --arg instance "$instance" '. + {lima_instance:$instance}' \
        "$host_run_dir/manifest.json" > "$host_run_dir/manifest.tmp"
      mv "$host_run_dir/manifest.tmp" "$host_run_dir/manifest.json"
    fi
    if [[ "${FIREWORK_E2E_KEEP:-0}" != 1 ]]; then
      limactl stop --force "$instance" >/dev/null 2>&1 || true
      limactl delete --force "$instance" >/dev/null 2>&1 || true
    else
      log "retaining Lima instance $instance"
    fi
  fi
  cleanup_bucket || status=1
  if [[ "${FIREWORK_E2E_KEEP:-0}" == 1 ]]; then
    log "retained host artifacts: $host_run_dir"
    log "retained bucket: $bucket"
  else
    log "local E2E artifacts: $host_run_dir"
  fi
  exit "$status"
}
trap cleanup_runtime EXIT INT TERM

log "creating real S3 bucket $bucket"
if [[ "$AWS_REGION" == us-east-1 ]]; then
  aws s3api create-bucket --bucket "$bucket" --region "$AWS_REGION" >/dev/null
else
  aws s3api create-bucket --bucket "$bucket" --region "$AWS_REGION" \
    --create-bucket-configuration "LocationConstraint=$AWS_REGION" >/dev/null
fi
bucket_created=1

if [[ "$mode" == linux ]]; then
  "$SCRIPT_DIR/check-env.sh"
  sudo_env="AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION,AWS_DEFAULT_REGION,AWS_EC2_METADATA_DISABLED,FIREWORK_E2E_KEEP,FIREWORK_E2E_TIMEOUT,FIREWORK_E2E_FIRECRACKER_BIN,FIREWORK_E2E_KERNEL,FIREWORK_E2E_ROOTFS"
  if [[ "$(id -u)" -eq 0 ]]; then
    bash "$GUEST_RUNNER" "$host_run_dir" "$bucket" "$AWS_REGION" "$commit" linux
  else
    require_cmd sudo
    sudo --preserve-env="$sudo_env" bash "$GUEST_RUNNER" "$host_run_dir" "$bucket" "$AWS_REGION" "$commit" linux
  fi
  status=$?
else
  require_cmd limactl
  [[ "$(uname -m)" == arm64 ]] || die "Lima mode currently requires an Apple Silicon host"

  instance="firework-e2e-$run_id"
  log "starting Lima instance $instance"
  limactl start --yes --name "$instance" --plain --vm-type=vz --nested-virt \
    --cpus 4 --memory 8 --disk 30 >/dev/null
  limactl shell --yes "$instance" -- sudo -n apt-get update -qq >/dev/null
  limactl shell --yes "$instance" -- sudo -n apt-get install -y -qq awscli curl e2fsprogs git iproute2 iptables jq openssl >/dev/null

  guest_root="$guest_run_dir"
  limactl shell --yes "$instance" -- mkdir -p "$guest_root/bin" "$guest_root/images" "$guest_root/scripts"
  limactl copy "$GUEST_RUNNER" "$instance:$guest_root/run-guest.sh"
  limactl copy "$SCRIPT_DIR/generate-pki.sh" "$instance:$guest_root/scripts/generate-pki.sh"
  limactl copy "$SCRIPT_DIR/collect-diagnostics.sh" "$instance:$guest_root/scripts/collect-diagnostics.sh"
  limactl copy "$REPO_ROOT/bin/firework-agent-linux-arm64" "$instance:$guest_root/bin/firework-agent-linux-arm64"
  limactl copy "$REPO_ROOT/bin/firework-controlplane-linux-arm64" "$instance:$guest_root/bin/firework-controlplane-linux-arm64"
  limactl copy "$REPO_ROOT/bin/fc-init-linux-arm64" "$instance:$guest_root/bin/fc-init-linux-arm64"
  limactl copy "$FIREWORK_E2E_FIRECRACKER_BIN" "$instance:$guest_root/images/firecracker"
  limactl copy "$FIREWORK_E2E_KERNEL" "$instance:$guest_root/images/vmlinux-source"
  limactl copy "$FIREWORK_E2E_ROOTFS" "$instance:$guest_root/images/rootfs-source.ext4"
  sudo_env="AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION,AWS_DEFAULT_REGION,AWS_EC2_METADATA_DISABLED,FIREWORK_E2E_KEEP,FIREWORK_E2E_TIMEOUT"
  LIMA_SHELLENV_ALLOW="+AWS_ACCESS_KEY_ID,+AWS_SECRET_ACCESS_KEY,+AWS_SESSION_TOKEN,+AWS_REGION,+AWS_DEFAULT_REGION" \
    limactl shell --yes --preserve-env "$instance" -- \
      sudo -n --preserve-env="$sudo_env" env \
        FIREWORK_E2E_FIRECRACKER_BIN="$guest_root/images/firecracker" \
        FIREWORK_E2E_KERNEL="$guest_root/images/vmlinux-source" \
        FIREWORK_E2E_ROOTFS="$guest_root/images/rootfs-source.ext4" \
        FIREWORK_E2E_SCRIPT_DIR="$guest_root/scripts" \
        bash "$guest_root/run-guest.sh" "$guest_root" "$bucket" "$AWS_REGION" "$commit" lima
  status=$?
fi
