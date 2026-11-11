#!/usr/bin/env bash
set -euo pipefail

OUT_DIR=${1:?usage: generate-pki.sh OUT_DIR}
mkdir -p "$OUT_DIR"
umask 077

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$OUT_DIR/ca.key" \
  -out "$OUT_DIR/ca.crt" \
  -days 2 \
  -subj "/CN=firework-local-e2e-ca" \
  >/dev/null 2>&1

openssl req -newkey rsa:2048 -nodes \
  -keyout "$OUT_DIR/controlplane.key" \
  -out "$OUT_DIR/controlplane.csr" \
  -subj "/CN=controlplane.local" \
  >/dev/null 2>&1

cat > "$OUT_DIR/controlplane.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:controlplane.local,IP:127.0.0.1,IP:10.254.0.1
EOF

openssl x509 -req \
  -in "$OUT_DIR/controlplane.csr" \
  -CA "$OUT_DIR/ca.crt" \
  -CAkey "$OUT_DIR/ca.key" \
  -CAcreateserial \
  -out "$OUT_DIR/controlplane.crt" \
  -days 2 \
  -extfile "$OUT_DIR/controlplane.ext" \
  >/dev/null 2>&1

rm -f "$OUT_DIR/controlplane.csr" "$OUT_DIR/controlplane.ext" "$OUT_DIR/ca.srl"
