#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "ERROR: $*" >&2
  exit 1
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    die "missing required command: $1"
  fi
}

if [[ "${1:-}" == "" ]]; then
  cat <<'EOF'
Usage:
  scripts/push-controlplane-image.sh <image-ref>

Example:
  scripts/push-controlplane-image.sh ghcr.io/artemnikitin/firework-controlplane:dev-test

Environment:
  PLATFORMS   Optional build platforms (default: linux/amd64,linux/arm64)
  VERSION     Optional version string (default: git describe --tags --always --dirty)
  COMMIT      Optional commit string (default: git rev-parse --short HEAD)
  BUILD_TIME  Optional UTC timestamp (default: now)
  BUILDER_NAME Optional buildx builder name for multi-arch (default: firework-multiarch)
EOF
  exit 1
fi

require_cmd docker

IMAGE_REF="$1"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_TIME="${BUILD_TIME:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}"
BUILDER_NAME="${BUILDER_NAME:-firework-multiarch}"

trimmed_platforms="$(echo "${PLATFORMS}" | tr -d '[:space:]')"
PLATFORMS="${trimmed_platforms}"
IFS=',' read -r -a platform_list <<< "${trimmed_platforms}"
multi_platform_requested=0
if (( ${#platform_list[@]} > 1 )); then
  multi_platform_requested=1
fi

builder_args=()
if (( multi_platform_requested == 1 )); then
  current_driver="$(docker buildx inspect --format '{{.Driver}}' 2>/dev/null || true)"
  if [[ "${current_driver}" != "docker-container" && "${current_driver}" != "kubernetes" && "${current_driver}" != "remote" ]]; then
    echo "Current buildx driver '${current_driver:-unknown}' does not support multi-platform builds."
    echo "Ensuring builder '${BUILDER_NAME}' with docker-container driver..."
    if ! docker buildx inspect "${BUILDER_NAME}" >/dev/null 2>&1; then
      docker buildx create --name "${BUILDER_NAME}" --driver docker-container >/dev/null
    fi
    docker buildx inspect --bootstrap "${BUILDER_NAME}" >/dev/null
    builder_args=(--builder "${BUILDER_NAME}")
  fi
fi

echo "Building and pushing ${IMAGE_REF}"
echo "Platforms: ${PLATFORMS}"
echo "Version: ${VERSION} Commit: ${COMMIT} BuildTime: ${BUILD_TIME}"

docker buildx build \
  "${builder_args[@]}" \
  --platform "${PLATFORMS}" \
  --file Dockerfile.controlplane \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "COMMIT=${COMMIT}" \
  --build-arg "BUILD_TIME=${BUILD_TIME}" \
  --tag "${IMAGE_REF}" \
  --push \
  .
