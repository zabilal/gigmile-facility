#!/usr/bin/env bash
# Post a signed payment notification, the way the provider does.
# Usage: ./scripts/pay.sh [customer] [naira-amount] [reference]
set -euo pipefail

SECRET="${HMAC_SECRET:-dev-secret-do-not-use-in-production}"
HOST="${HOST:-http://localhost:8080}"

CUSTOMER="${1:-GIG000001}"
AMOUNT="${2:-10000}"
REFERENCE="${3:-VPAY$(date +%s%N | cut -c1-17)}"

BODY=$(printf '{"customer_id":"%s","payment_status":"COMPLETE","transaction_amount":"%s","transaction_date":"%s","transaction_reference":"%s"}' \
  "$CUSTOMER" "$AMOUNT" "$(date '+%Y-%m-%d %H:%M:%S')" "$REFERENCE")

TIMESTAMP=$(date +%s)
SIGNATURE=$(printf '%s.%s' "$TIMESTAMP" "$BODY" \
  | openssl dgst -sha256 -hmac "$SECRET" -hex \
  | awk '{print $NF}')

curl -sS -X POST "$HOST/v1/payments" \
  -H 'Content-Type: application/json' \
  -H "X-Timestamp: $TIMESTAMP" \
  -H "X-Signature: $SIGNATURE" \
  -d "$BODY"
echo
