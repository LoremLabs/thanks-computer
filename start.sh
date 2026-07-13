#!/usr/bin/env bash
# Boot the chassis locally.
# Idempotent: only does the steps that haven't happened yet.
# Pass extra chassis flags through: ./start.sh --log-level=debug
set -euo pipefail

cd "$(dirname "$0")"

if [[ ! -f .env ]]; then
  echo "[start] creating .env from .env.tmpl"
  cp .env.tmpl .env
fi

if [[ ! -x chassis/bin/txco ]]; then
  echo "[start] building txco (GOWORK=off)"
  GOWORK=off make build
fi

# TCP default :5050 
echo "[start] chassis listening: web :8080  tcp :5050"
exec ./chassis/bin/txco serve \
  --env=dev \
  --web-addr=:8080 \
  --tcp-listen-addrs=:5050 \
  --egress-policy=open \
  "$@"
