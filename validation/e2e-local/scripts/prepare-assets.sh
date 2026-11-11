#!/usr/bin/env bash
set -Eeuo pipefail

RUN_DIR=${1:?usage: prepare-assets.sh RUN_DIR}
IMAGE_DIR="$RUN_DIR/images"
BIN_DIR="$RUN_DIR/bin"
FIRECRACKER_VERSION="${FIREWORK_E2E_FIRECRACKER_VERSION:-1.12.0}"
FIRECRACKER_ARCH="${FIREWORK_E2E_FIRECRACKER_ARCH:-aarch64}"
FIRECRACKER_TARBALL="firecracker-v${FIRECRACKER_VERSION}-${FIRECRACKER_ARCH}.tgz"
FIRECRACKER_SHA256="${FIREWORK_E2E_FIRECRACKER_SHA256:-55f3e76c6a16128e91aea1d2ed3d436f5d4e2e9547bfdd226ce570a89cd48921}"
KERNEL_KEY="${FIREWORK_E2E_KERNEL_KEY:-firecracker-ci/v1.12/aarch64/vmlinux-5.10.233}"

log() {
  printf '==> %s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

mkdir -p "$IMAGE_DIR" "$BIN_DIR"

if [[ ! -x "$BIN_DIR/firecracker" ]]; then
  tmp_dir="$(mktemp -d "$RUN_DIR/firecracker-download.XXXXXX")"
  trap 'rm -rf "$tmp_dir"' EXIT
  archive="$tmp_dir/$FIRECRACKER_TARBALL"
  url="https://github.com/firecracker-microvm/firecracker/releases/download/v${FIRECRACKER_VERSION}/${FIRECRACKER_TARBALL}"
  log "downloading pinned Firecracker $FIRECRACKER_VERSION ($FIRECRACKER_ARCH)"
  curl --fail --silent --show-error --location "$url" --output "$archive"
  printf '%s  %s\n' "$FIRECRACKER_SHA256" "$archive" | sha256sum --check --status - \
    || die "Firecracker archive checksum mismatch: $url"
  tar --extract --gzip --file "$archive" --directory "$tmp_dir"
  extracted="$tmp_dir/release-v${FIRECRACKER_VERSION}-${FIRECRACKER_ARCH}/firecracker-v${FIRECRACKER_VERSION}-${FIRECRACKER_ARCH}"
  [[ -x "$extracted" ]] || die "Firecracker archive did not contain $extracted"
  install -m 0755 "$extracted" "$BIN_DIR/firecracker"
fi

[[ -x "$BIN_DIR/firecracker" ]] || die "Firecracker is not executable: $BIN_DIR/firecracker"

kernel="$IMAGE_DIR/vmlinux"
if [[ ! -r "$kernel" ]]; then
  log "downloading pinned Firecracker kernel $KERNEL_KEY"
  curl --fail --silent --show-error --location \
    "https://s3.amazonaws.com/spec.ccfc.min/$KERNEL_KEY" --output "$kernel"
  chmod 0644 "$kernel"
fi

[[ -s "$kernel" ]] || die "kernel is empty: $kernel"

firecracker_version="$("$BIN_DIR/firecracker" --version 2>&1 | head -n 1)"
kernel_sha256="$(sha256sum "$kernel" | awk '{print $1}')"
firecracker_sha256="$(sha256sum "$BIN_DIR/firecracker" | awk '{print $1}')"
cat > "$RUN_DIR/assets-manifest.json" <<EOF
{
  "firecracker_version": "$FIRECRACKER_VERSION",
  "firecracker_arch": "$FIRECRACKER_ARCH",
  "firecracker_release_sha256": "$FIRECRACKER_SHA256",
  "firecracker_binary_sha256": "$firecracker_sha256",
  "firecracker_reported_version": "$firecracker_version",
  "kernel_key": "$KERNEL_KEY",
  "kernel_sha256": "$kernel_sha256"
}
EOF

log "prepared Firecracker and kernel assets"
