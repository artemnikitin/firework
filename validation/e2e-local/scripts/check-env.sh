#!/usr/bin/env bash
set -euo pipefail

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
require_cmd jq
require_cmd openssl

[[ -n "${AWS_REGION:-${AWS_DEFAULT_REGION:-}}" ]] || die "AWS_REGION or AWS_DEFAULT_REGION is required"

case "$(uname -s)" in
  Darwin)
    require_cmd limactl
    ;;
  Linux)
    require_cmd ip
    require_cmd iptables
    [[ -r /dev/kvm && -w /dev/kvm ]] || die "/dev/kvm is not readable and writable"
    [[ -e /dev/net/tun ]] || die "/dev/net/tun is required"
    ;;
  *)
    die "unsupported host OS: $(uname -s)"
    ;;
esac
