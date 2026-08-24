#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 BASE_URL" >&2
  exit 2
fi

base_url="${1%/}"
health="$(curl --fail --silent --show-error "$base_url/healthz")"
ready="$(curl --fail --silent --show-error "$base_url/readyz")"
metrics="$(curl --fail --silent --show-error "$base_url/metrics")"

printf '%s' "$health" | grep -q '"status":"ok"'
printf '%s' "$ready" | grep -q '"status":"ready"'
printf '%s' "$metrics" | grep -q '^tintwire_http_requests_total '

echo "Tintwire ingress is live, writable, and exporting metrics at $base_url"
