#!/usr/bin/env bash
# Simulates Stripe POSTing a signed `checkout.session.completed` webhook
# to the chassis. It signs `<t>.<body>` with the SAME value you stored
# as the STRIPE_WEBHOOK_SECRET secret, so the chassis's hmac-verify
# passes. Change SECRET to mismatch and you'll get a 401.
#
# Usage:
#   STRIPE_WEBHOOK_SECRET=whsec_demo_secret ./send-webhook.sh
#   ./send-webhook.sh http://localhost:8080/webhooks/stripe   # custom URL
#
# Requires: bash, curl, openssl.
set -euo pipefail

SECRET="${STRIPE_WEBHOOK_SECRET:-whsec_demo_secret}"
URL="${1:-http://localhost:8080/webhooks/stripe}"
BODY='{"type":"checkout.session.completed","data":{"object":{"customer":"cus_demo123"}}}'
T="$(date +%s)"

# Stripe's signed payload is the literal string "<timestamp>.<raw body>".
SIG="$(printf '%s.%s' "$T" "$BODY" \
  | openssl dgst -sha256 -hmac "$SECRET" \
  | sed 's/^.*= *//')"

echo "POST $URL"
echo "  Stripe-Signature: t=$T,v1=${SIG:0:16}…"
echo
curl -sS -i -X POST "$URL" \
  -H "Content-Type: application/json" \
  -H "Stripe-Signature: t=$T,v1=$SIG" \
  --data "$BODY"
echo
