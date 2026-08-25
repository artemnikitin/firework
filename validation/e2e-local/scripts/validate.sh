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

if [[ -z "${AWS_ACCESS_KEY_ID:-}" ]]; then
  aws_profile="${AWS_PROFILE:-${FIREWORK_E2E_AWS_PROFILE:-artemnikitin}}"
  eval "$(aws configure export-credentials --profile "$aws_profile" --format env)"
  unset AWS_PROFILE
fi

export AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-}}"
export AWS_DEFAULT_REGION="$AWS_REGION"
[[ -n "$AWS_REGION" ]] || die "AWS_REGION or AWS_DEFAULT_REGION is required"
aws sts get-caller-identity >/dev/null || die "AWS credentials are not usable"

images_bucket="${FIREWORK_E2E_IMAGES_BUCKET:-artemnikitin-firework-images}"
[[ "$images_bucket" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] \
  || die "FIREWORK_E2E_IMAGES_BUCKET is not a valid S3 bucket name"

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

if [[ "$mode" == lima ]]; then
  [[ "$(uname -m)" == arm64 ]] || die "Lima mode requires an Apple Silicon host"
  require_cmd limactl
  log "host capability probe: macOS $(sw_vers -productVersion), Lima $(limactl --version | head -n 1)"
  log "the guest probe will verify /dev/kvm, /dev/net/tun, namespaces, bridges, and iptables"
fi

aws s3api head-object --bucket "$images_bucket" --key "${FIREWORK_E2E_ES_IMAGE_KEY:-tenant-2-elasticsearch-rootfs.ext4}" >/dev/null \
  || die "cannot read the Elasticsearch rootfs from s3://$images_bucket (set FIREWORK_E2E_IMAGES_BUCKET or image key overrides)"
aws s3api head-object --bucket "$images_bucket" --key "${FIREWORK_E2E_KIBANA_IMAGE_KEY:-tenant-2-kibana-rootfs.ext4}" >/dev/null \
  || die "cannot read the Kibana rootfs from s3://$images_bucket (set FIREWORK_E2E_IMAGES_BUCKET or image key overrides)"

log "building Linux Firework binaries"
(cd "$REPO_ROOT" && make "build-linux-$host_arch" >/dev/null)

commit="$(git -C "$REPO_ROOT" rev-parse HEAD)"
if [[ -n "${FIREWORK_E2E_WORKDIR:-}" ]]; then
  host_run_dir="$FIREWORK_E2E_WORKDIR"
else
  host_run_dir="$(mktemp -d "${TMPDIR:-/tmp}/firework-e2e-local.XXXXXX")"
fi
mkdir -p "$host_run_dir"

conditional_key="probe/conditional-write-$run_id"
conditional_body="$host_run_dir/conditional-write.txt"
conditional_updated_body="$host_run_dir/conditional-write-updated.txt"
printf 'local-e2e-conditional-write\n' > "$conditional_body"
printf 'local-e2e-conditional-write-updated\n' > "$conditional_updated_body"

run_conditional_write_probe() {
  local first_etag second_etag
  aws s3api put-object --bucket "$bucket" --key "$conditional_key" \
    --body "$conditional_body" --if-none-match '*' >/dev/null
  if aws s3api put-object --bucket "$bucket" --key "$conditional_key" \
    --body "$conditional_body" --if-none-match '*' >/dev/null 2>&1; then
    die "S3 If-None-Match conditional write unexpectedly overwrote an object"
  fi
  first_etag="$(aws s3api head-object --bucket "$bucket" --key "$conditional_key" --query ETag --output text)"
  aws s3api put-object --bucket "$bucket" --key "$conditional_key" \
    --body "$conditional_updated_body" --if-match "$first_etag" >/dev/null
  second_etag="$(aws s3api head-object --bucket "$bucket" --key "$conditional_key" --query ETag --output text)"
  if aws s3api put-object --bucket "$bucket" --key "$conditional_key" \
    --body "$conditional_body" --if-match "$first_etag" >/dev/null 2>&1; then
    die "S3 stale If-Match conditional write unexpectedly succeeded"
  fi
  jq -n --arg key "$conditional_key" --arg first "$first_etag" --arg second "$second_etag" \
    '{key:$key,if_none_match:"passed",if_match:"passed",stale_if_match:"passed",first_etag:$first,second_etag:$second}' \
    > "$host_run_dir/conditional-write.json"
}

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
run_conditional_write_probe

if [[ "$mode" == linux ]]; then
  "$SCRIPT_DIR/check-env.sh"
  sudo_env="AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION,AWS_DEFAULT_REGION,AWS_EC2_METADATA_DISABLED,FIREWORK_E2E_KEEP,FIREWORK_E2E_TIMEOUT,FIREWORK_E2E_HEALTH_RETRIES,FIREWORK_E2E_IMAGES_BUCKET,FIREWORK_E2E_ES_IMAGE_KEY,FIREWORK_E2E_KIBANA_IMAGE_KEY,FIREWORK_E2E_FIRECRACKER_VERSION,FIREWORK_E2E_KERNEL_KEY,FIREWORK_E2E_SERVICE_VCPUS,FIREWORK_E2E_SERVICE_MEMORY_MB,FIREWORK_E2E_ES_VCPUS,FIREWORK_E2E_ES_MEMORY_MB,FIREWORK_E2E_KIBANA_VCPUS,FIREWORK_E2E_KIBANA_MEMORY_MB,FIREWORK_E2E_ES_JAVA_OPTS,FIREWORK_E2E_ENABLE_VOLUME,FIREWORK_E2E_VOLUME_SIZE,FIREWORK_E2E_STORAGE_CAPACITY"
  if [[ "$(id -u)" -eq 0 ]]; then
    bash "$GUEST_RUNNER" "$host_run_dir" "$bucket" "$AWS_REGION" "$commit" linux "$images_bucket"
  else
    require_cmd sudo
    sudo --preserve-env="$sudo_env" bash "$GUEST_RUNNER" "$host_run_dir" "$bucket" "$AWS_REGION" "$commit" linux "$images_bucket"
  fi
  status=$?
else
  require_cmd limactl
  [[ "$(uname -m)" == arm64 ]] || die "Lima mode currently requires an Apple Silicon host"

  instance="firework-e2e-$run_id"
  log "starting Lima instance $instance"
  lima_cpus="${FIREWORK_E2E_LIMA_CPUS:-8}"
  lima_memory="${FIREWORK_E2E_LIMA_MEMORY_GB:-12}"
  lima_disk="${FIREWORK_E2E_LIMA_DISK_GB:-60}"
  limactl start --yes --name "$instance" --plain --vm-type=vz --nested-virt \
    --cpus "$lima_cpus" --memory "$lima_memory" --disk "$lima_disk" >/dev/null
  limactl shell --yes "$instance" -- sudo -n apt-get update -qq >/dev/null
  limactl shell --yes "$instance" -- sudo -n apt-get install -y -qq awscli curl e2fsprogs git iproute2 iptables jq openssl >/dev/null

  guest_root="$guest_run_dir"
  limactl shell --yes "$instance" -- mkdir -p "$guest_root/bin" "$guest_root/images" "$guest_root/scripts"
  limactl copy "$GUEST_RUNNER" "$instance:$guest_root/run-guest.sh"
  limactl copy "$SCRIPT_DIR/generate-pki.sh" "$instance:$guest_root/scripts/generate-pki.sh"
  limactl copy "$SCRIPT_DIR/prepare-assets.sh" "$instance:$guest_root/scripts/prepare-assets.sh"
  limactl copy "$SCRIPT_DIR/collect-diagnostics.sh" "$instance:$guest_root/scripts/collect-diagnostics.sh"
  limactl copy "$REPO_ROOT/bin/firework-agent-linux-arm64" "$instance:$guest_root/bin/firework-agent-linux-arm64"
  limactl copy "$REPO_ROOT/bin/firework-controlplane-linux-arm64" "$instance:$guest_root/bin/firework-controlplane-linux-arm64"
  limactl copy "$REPO_ROOT/bin/fc-init-linux-arm64" "$instance:$guest_root/bin/fc-init-linux-arm64"
  if [[ -n "${FIREWORK_E2E_FIRECRACKER_BIN:-}" ]]; then
    [[ -x "$FIREWORK_E2E_FIRECRACKER_BIN" ]] || die "FIREWORK_E2E_FIRECRACKER_BIN is not executable"
    limactl copy "$FIREWORK_E2E_FIRECRACKER_BIN" "$instance:$guest_root/bin/firecracker"
  fi
  if [[ -n "${FIREWORK_E2E_KERNEL:-}" ]]; then
    [[ -r "$FIREWORK_E2E_KERNEL" ]] || die "FIREWORK_E2E_KERNEL is not readable"
    limactl copy "$FIREWORK_E2E_KERNEL" "$instance:$guest_root/images/vmlinux"
  fi
  sudo_env="AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION,AWS_DEFAULT_REGION,AWS_EC2_METADATA_DISABLED,FIREWORK_E2E_KEEP,FIREWORK_E2E_TIMEOUT,FIREWORK_E2E_HEALTH_RETRIES,FIREWORK_E2E_IMAGES_BUCKET,FIREWORK_E2E_ES_IMAGE_KEY,FIREWORK_E2E_KIBANA_IMAGE_KEY,FIREWORK_E2E_FIRECRACKER_VERSION,FIREWORK_E2E_KERNEL_KEY,FIREWORK_E2E_SERVICE_VCPUS,FIREWORK_E2E_SERVICE_MEMORY_MB,FIREWORK_E2E_ES_VCPUS,FIREWORK_E2E_ES_MEMORY_MB,FIREWORK_E2E_KIBANA_VCPUS,FIREWORK_E2E_KIBANA_MEMORY_MB,FIREWORK_E2E_ES_JAVA_OPTS,FIREWORK_E2E_ENABLE_VOLUME,FIREWORK_E2E_VOLUME_SIZE,FIREWORK_E2E_STORAGE_CAPACITY"
  LIMA_SHELLENV_ALLOW="AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_SESSION_TOKEN,AWS_REGION,AWS_DEFAULT_REGION,AWS_EC2_METADATA_DISABLED,FIREWORK_E2E_KEEP,FIREWORK_E2E_TIMEOUT,FIREWORK_E2E_HEALTH_RETRIES,FIREWORK_E2E_IMAGES_BUCKET,FIREWORK_E2E_ES_IMAGE_KEY,FIREWORK_E2E_KIBANA_IMAGE_KEY,FIREWORK_E2E_FIRECRACKER_VERSION,FIREWORK_E2E_KERNEL_KEY,FIREWORK_E2E_SERVICE_VCPUS,FIREWORK_E2E_SERVICE_MEMORY_MB,FIREWORK_E2E_ES_VCPUS,FIREWORK_E2E_ES_MEMORY_MB,FIREWORK_E2E_KIBANA_VCPUS,FIREWORK_E2E_KIBANA_MEMORY_MB,FIREWORK_E2E_ES_JAVA_OPTS,FIREWORK_E2E_ENABLE_VOLUME,FIREWORK_E2E_VOLUME_SIZE,FIREWORK_E2E_STORAGE_CAPACITY" \
    limactl shell --yes --preserve-env "$instance" -- \
      sudo -n --preserve-env="$sudo_env" env \
        FIREWORK_E2E_FIRECRACKER_BIN="$guest_root/images/firecracker" \
        FIREWORK_E2E_KERNEL="$guest_root/images/vmlinux" \
        FIREWORK_E2E_FIRECRACKER_VERSION="${FIREWORK_E2E_FIRECRACKER_VERSION:-1.12.0}" \
        FIREWORK_E2E_KERNEL_KEY="${FIREWORK_E2E_KERNEL_KEY:-firecracker-ci/v1.12/aarch64/vmlinux-5.10.233}" \
        FIREWORK_E2E_IMAGES_BUCKET="$images_bucket" \
        FIREWORK_E2E_ES_IMAGE_KEY="${FIREWORK_E2E_ES_IMAGE_KEY:-tenant-2-elasticsearch-rootfs.ext4}" \
        FIREWORK_E2E_KIBANA_IMAGE_KEY="${FIREWORK_E2E_KIBANA_IMAGE_KEY:-tenant-2-kibana-rootfs.ext4}" \
        FIREWORK_E2E_SCRIPT_DIR="$guest_root/scripts" \
        FIREWORK_E2E_ASSET_SCRIPT="$guest_root/scripts/prepare-assets.sh" \
        bash "$guest_root/run-guest.sh" "$guest_root" "$bucket" "$AWS_REGION" "$commit" lima "$images_bucket"
  status=$?
fi
