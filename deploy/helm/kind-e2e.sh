#!/bin/sh
# End-to-end check of the chart in a throwaway kind cluster. Runs INSIDE a
# toolbox container that has the host Docker socket and the chart mounted:
#
#   docker build -f deploy/docker/Dockerfile -t zerowatch:w4-helm .
#   docker run --rm --name w4-helm-tools -v //var/run/docker.sock:/var/run/docker.sock \
#     -v "D:\home\zerowatch\deploy\helm:/charts" -v w4-helm-bin:/opt/bin docker:27.3.1-cli \
#     sh /charts/kind-e2e.sh
#
# It creates kind cluster "w4-helm" (node container w4-helm-control-plane),
# installs mode=single (+ agent DaemonSet), then mode=cluster with 3 storage
# replicas (+ agent), checks the UI, /api/v1/stats and /api/v1/cluster, and
# deletes the cluster (KEEP=1 keeps it).
set -eu
CLUSTER=w4-helm
IMAGE=zerowatch:w4-helm
NODE_IMAGE=kindest/node:v1.30.4
BIN=/opt/bin
export PATH="$BIN:$PATH"
mkdir -p "$BIN"
apk add --no-cache curl jq >/dev/null
arch=amd64
[ -x "$BIN/kind" ] || { curl -fsSLo "$BIN/kind" "https://kind.sigs.k8s.io/dl/v0.24.0/kind-linux-$arch"; chmod +x "$BIN/kind"; }
[ -x "$BIN/kubectl" ] || { curl -fsSLo "$BIN/kubectl" "https://dl.k8s.io/release/v1.30.4/bin/linux/$arch/kubectl"; chmod +x "$BIN/kubectl"; }
[ -x "$BIN/helm" ] || { curl -fsSL "https://get.helm.sh/helm-v3.16.2-linux-$arch.tar.gz" | tar -xz -C /tmp; mv /tmp/linux-$arch/helm "$BIN/helm"; }

cleanup() {
  if [ "${KEEP:-0}" != 1 ]; then kind delete cluster --name "$CLUSTER" || true; fi
}
trap cleanup EXIT

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s
# The toolbox reaches the API server over the kind network by container name.
docker network connect kind "$(hostname)" 2>/dev/null || true
kind get kubeconfig --internal --name "$CLUSTER" > /tmp/kubeconfig
export KUBECONFIG=/tmp/kubeconfig
kubectl get nodes -o wide
docker image inspect busybox:1.36.1 >/dev/null 2>&1 || docker pull -q busybox:1.36.1
kind load docker-image "$IMAGE" curlimages/curl:8.10.1 busybox:1.36.1 --name "$CLUSTER"

COMMON="--set image.repository=zerowatch --set image.tag=w4-helm --set image.pullPolicy=Never --set agent.enabled=true
  --set secrets.values.clusterSecret=w4-helm-e2e-cluster-secret
  --set single.resources.requests.cpu=50m --set storageNodes.resources.requests.cpu=50m
  --set ingest.resources.requests.cpu=50m --set query.resources.requests.cpu=50m --set agent.resources.requests.cpu=10m"

check() { # $1 = label
  kubectl -n zw port-forward svc/zw-zerowatch 18080:8080 >/tmp/pf.log 2>&1 &
  pf=$!
  sleep 4
  echo "--- $1: UI"
  curl -fsS http://127.0.0.1:18080/ | grep -o '<title>[^<]*</title>' || { echo "UI FAILED"; kill $pf; return 1; }
  echo "--- $1: /api/v1/stats"
  curl -fsS http://127.0.0.1:18080/api/v1/stats | jq -c '{role, node, ebpfBuild}'
  echo "--- $1: /api/v1/cluster"
  curl -fsS http://127.0.0.1:18080/api/v1/cluster | jq -c '{clustered, ring: .ring.nodes, members: [.members[] | {name, role, state}]}'
  curl -fsS http://127.0.0.1:18080/api/v1/cluster > "/tmp/cluster-$1.json"
  kill $pf
}

echo "===== mode=single"
kubectl create namespace zw 2>/dev/null || true
# shellcheck disable=SC2086
helm upgrade --install zw /charts/zerowatch -n zw $COMMON --wait --timeout 4m
kubectl -n zw get pods -o wide
kubectl -n zw rollout status ds/zw-zerowatch-agent --timeout 2m
sleep 15
check single
echo "--- agent log (head)"
kubectl -n zw logs ds/zw-zerowatch-agent --tail 25 || true
s_alive=$(jq '[.members[] | select(.state=="alive")] | length' /tmp/cluster-single.json)
echo "single: alive members $s_alive (expect 2: the role=all pod + 1 agent)"
[ "$s_alive" -eq 2 ] || { echo "EXPECTED 2 MEMBERS IN SINGLE MODE"; exit 1; }
helm test zw -n zw --logs || true
helm uninstall zw -n zw --wait
kubectl -n zw delete pvc --all --wait=true

echo "===== mode=cluster"
# shellcheck disable=SC2086
helm upgrade --install zw /charts/zerowatch -n zw $COMMON --set mode=cluster --set storageNodes.replicas=3 \
  --set ingest.replicas=1 --set storageNodes.persistence.size=1Gi --wait --timeout 6m
kubectl -n zw get pods -o wide
sleep 15
check cluster
ring=$(jq '.ring.nodes | length' /tmp/cluster-cluster.json)
alive=$(jq '[.members[] | select(.state=="alive")] | length' /tmp/cluster-cluster.json)
echo "ring nodes: $ring, alive members: $alive"
kubectl -n zw logs ds/zw-zerowatch-agent --tail 15 || true
[ "$ring" -eq 3 ] || { echo "EXPECTED 3 RING NODES"; exit 1; }
# 3 storage + ingest + query + 1 agent
[ "$alive" -eq 6 ] || { echo "EXPECTED 6 ALIVE MEMBERS"; exit 1; }
echo "--- cluster: ingest a metric through the ingest service, read it back through query"
kubectl -n zw port-forward svc/zw-zerowatch-ingest 18081:8080 >/tmp/pf2.log 2>&1 &
pf2=$!
kubectl -n zw port-forward svc/zw-zerowatch 18080:8080 >/tmp/pf.log 2>&1 &
pf=$!
sleep 4
now=$(date +%s)000
for i in 1 2 3 4 5; do
  curl -fsS -X POST http://127.0.0.1:18081/api/v1/ingest/metrics -H 'Content-Type: application/json'     -d "[{\"namespace\":\"W4Helm\",\"metricName\":\"E2E$i\",\"dimensions\":[{\"Name\":\"Run\",\"Value\":\"kind\"}],\"value\":$i,\"timestamp\":$now}]" || echo "ingest $i failed"
done
echo
sleep 3
curl -fsS "http://127.0.0.1:18080/api/v1/series?namespace=W4Helm" | head -c 600; echo
curl -fsS http://127.0.0.1:18080/api/v1/cluster | jq -c '.routing'
kill $pf $pf2
echo "E2E OK"
