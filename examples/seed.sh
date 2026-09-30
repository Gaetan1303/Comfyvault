#!/usr/bin/env sh
# Loads a few example blocks and one preset into a running comfyvault.
# Usage: examples/seed.sh [base-url]   (default http://127.0.0.1:8080)
# With an API key: COMFYVAULT_API_KEY=... examples/seed.sh
set -eu

BASE="${1:-http://127.0.0.1:8080}"

post() {
  if [ -n "${COMFYVAULT_API_KEY:-}" ]; then
    curl -fsS -X POST "$BASE$1" -H 'Content-Type: application/json' \
      -H "Authorization: Bearer $COMFYVAULT_API_KEY" -d "$2"
  else
    curl -fsS -X POST "$BASE$1" -H 'Content-Type: application/json' -d "$2"
  fi
  echo
}

post /api/v1/blocks '{
  "id": "quality-header",
  "kind": "quality",
  "text": "masterpiece, best quality, amazing quality, very aesthetic, absurdres",
  "tags": ["quality"]
}'

post /api/v1/blocks '{
  "id": "subject",
  "kind": "subject",
  "text": "1girl, solo, {{hair}} hair, {{outfit}}",
  "tags": ["character"]
}'

post /api/v1/blocks '{
  "id": "negative-base",
  "kind": "negative",
  "text": "worst quality, low quality, bad anatomy, watermark, signature",
  "tags": ["negative"]
}'

post /api/v1/presets '{
  "id": "portrait",
  "name": "Portrait",
  "blocks": ["quality-header", "subject"]
}'

post /api/v1/presets '{
  "id": "negative-default",
  "name": "Default negative",
  "blocks": ["negative-base"]
}'
