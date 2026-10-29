#!/bin/bash
set -e

# Number of remote clusters to create (default: 1 when unspecified).
NUM_REMOTES=${1:-1}

REMOTE_CLUSTERS=("cluster-remote")
if [ "$NUM_REMOTES" -ge 2 ]; then
  REMOTE_CLUSTERS=("cluster-remote" "cluster-alpha")
fi

echo -e "\033[1;36m>>> [Setup] Verifica requisiti: $NUM_REMOTES cluster remoto/i (${REMOTE_CLUSTERS[*]})\033[0m"

# 0. Check whether the virtual nodes are already active and ready.
if kubectl config use-context kind-cluster-local >/dev/null 2>&1; then
  CURRENT_V_NODES=$(kubectl get nodes -l liqo.io/type=virtual-node --no-headers 2>/dev/null | grep -i "Ready" | wc -l || echo 0)
  if [ "$CURRENT_V_NODES" -ge "$NUM_REMOTES" ]; then
    echo -e "\033[1;32m[Setup] $CURRENT_V_NODES nodo/i virtuale/i già Ready! Setup bypassato, avvio test immediato.\033[0m"
    exit 0
  fi
fi

echo -e "\n\033[1;33m[Setup] Infrastruttura mancante o incompleta. Avvio creazione...\033[0m"

echo -e "\n\033[1;32mStep 1: Creating local and remote Kind clusters...\033[0m"
if ! kind get clusters | grep -q "^cluster-local$"; then kind create cluster --name cluster-local; fi
for rc in "${REMOTE_CLUSTERS[@]}"; do
  if ! kind get clusters | grep -q "^$rc$"; then kind create cluster --name "$rc"; fi
done

echo -e "\n\033[1;32mStep 1.5: Installing Prometheus CRDs...\033[0m"
kubectl --context kind-cluster-local apply --server-side -f https://github.com/prometheus-operator/prometheus-operator/releases/download/v0.72.0/stripped-down-crds.yaml >/dev/null 2>&1 || true
for rc in "${REMOTE_CLUSTERS[@]}"; do
  kubectl --context "kind-$rc" apply --server-side -f https://github.com/prometheus-operator/prometheus-operator/releases/download/v0.72.0/stripped-down-crds.yaml >/dev/null 2>&1 || true
done

echo -e "\n\033[1;32mStep 2: Installing Liqo (v1.0+) with metrics enabled...\033[0m"
liqoctl install kind --cluster-id cluster-local --context kind-cluster-local --enable-metrics >/dev/null 2>&1 || true
for rc in "${REMOTE_CLUSTERS[@]}"; do
  liqoctl install kind --cluster-id "$rc" --context "kind-$rc" --enable-metrics >/dev/null 2>&1 || true
done

echo -e "\n\033[1;32mStep 3: Peering clusters using NodePort...\033[0m"
for rc in "${REMOTE_CLUSTERS[@]}"; do
  liqoctl peer --context kind-cluster-local --remote-context "kind-$rc" --gw-server-service-type NodePort >/dev/null 2>&1 || true
done

echo -e "\n\033[1;32mWaiting for virtual nodes to become Ready...\033[0m"
kubectl config use-context kind-cluster-local
for rc in "${REMOTE_CLUSTERS[@]}"; do
  echo "Waiting for node $rc..."
  while ! kubectl get nodes | grep -q "^$rc"; do sleep 2; done
  kubectl wait --for=condition=Ready "node/$rc" --timeout=180s >/dev/null
done

echo -e "\n\033[1;32mSetup complete! Virtual nodes are ready.\033[0m"