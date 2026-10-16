#!/usr/bin/env bash
# Lint the chart and validate the rendered manifests with kubeconform, all in
# Docker. Run from Git Bash or any POSIX shell: bash deploy/helm/verify.sh
set -euo pipefail
export MSYS_NO_PATHCONV=1
HERE="$(cd "$(dirname "$0")" && pwd)"
# Docker Desktop on Windows wants a Windows path for the bind mount.
if command -v cygpath >/dev/null 2>&1; then MOUNT="$(cygpath -w "$HERE")"; else MOUNT="$HERE"; fi
HELM="docker run --rm -v ${MOUNT}:/charts alpine/helm:3.16.2"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

# name|--set flags
CASES=(
  "single|"
  "single-agent|--set agent.enabled=true --set forecaster.enabled=true --set networkPolicy.enabled=true --set ingress.enabled=true --set secrets.values.apiKeys=0123456789abcdef:1"
  "cluster|--set mode=cluster"
  "cluster-full|--set mode=cluster --set agent.enabled=true --set agent.privileged=true --set agent.hostNetwork=false --set kafka.enabled=true --set kafka.brokers={redpanda:9092} --set forecaster.enabled=true --set ingest.autoscaling.enabled=true --set query.autoscaling.enabled=true --set networkPolicy.enabled=true --set networkPolicy.nodeCIDRs={10.0.0.0/8} --set ingress.enabled=true --set secrets.existingSecret=zw --set metadata.dsn=postgres://zw@pg/zw --set storage.backend=clickhouse --set storageNodes.persistence.enabled=false"
  "cluster-servicemonitor|--set mode=cluster --set serviceMonitor.enabled=true"
)

fail=0
for c in "${CASES[@]}"; do
  name="${c%%|*}"; flags="${c#*|}"
  echo "== $name"
  # shellcheck disable=SC2086
  $HELM lint /charts/zerowatch $flags | grep -E "linted|ERROR|WARNING" || fail=1
  # shellcheck disable=SC2086
  $HELM template zw /charts/zerowatch --namespace zw $flags > "$OUT/$name.yaml" || { fail=1; continue; }
  echo "   $(grep -c '^kind:' "$OUT/$name.yaml") objects"
done

if command -v cygpath >/dev/null 2>&1; then OUTM="$(cygpath -w "$OUT")"; else OUTM="$OUT"; fi
for f in "$OUT"/*.yaml; do
  n="$(basename "$f")"
  echo "== kubeconform $n"
  docker run --rm -v "${OUTM}:/m" ghcr.io/yannh/kubeconform:v0.6.7 -strict -summary \
    -kubernetes-version 1.30.0 \
    -schema-location default \
    -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
    "/m/$n" || fail=1
done
exit $fail
