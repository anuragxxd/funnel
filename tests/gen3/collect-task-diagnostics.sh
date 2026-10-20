#!/usr/bin/env bash
set -u

: "${NAMESPACE:?}"
namespace="workflow-pods-$NAMESPACE"
printf 'Gen3 task diagnostics collector started for %s\n' "$namespace"

for ((sample = 0; sample < 40; sample++)); do
  sleep 30
  printf '\nGen3 task diagnostics %s\n' "$(date -u +%FT%TZ)"
  kubectl --request-timeout=10s -n "$namespace" get pods -o wide || true
  kubectl --request-timeout=10s -n "$namespace" get events \
    --field-selector type=Warning --sort-by='.lastTimestamp' || true
  while read -r pod; do
    [[ -n "$pod" ]] || continue
    kubectl --request-timeout=10s -n "$namespace" logs "$pod" \
      --all-containers --prefix --tail=60 2>&1 |
      sed -E 's/[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]+/[REDACTED JWT]/g' || true
  done < <(kubectl --request-timeout=10s -n "$namespace" get pods -o name 2>/dev/null)
done
