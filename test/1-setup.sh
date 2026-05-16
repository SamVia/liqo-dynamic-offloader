#!/bin/bash
set -e

echo "\033[1;32m Step 1: Creating local and remote Kind clusters...\033[0m"
kind create cluster --name cluster-local || true
kind create cluster --name cluster-remote || true

echo "\033[1;32m Step 2: Installing Liqo (v1.0+)...\033[0m"
kubectl config use-context kind-cluster-local
liqoctl install kind --cluster-id cluster-local

kubectl config use-context kind-cluster-remote
liqoctl install kind --cluster-id cluster-remote

echo "\033[1;32m Step 3: Peering clusters using NodePort...\033[0m"
liqoctl peer --context kind-cluster-local --remote-context kind-cluster-remote --gw-server-service-type NodePort

echo "\033[1;32mWaiting for Liqo virtual node to be created by the controller...\033[0m"
while [ -z "$(kubectl get nodes -l liqo.io/type=virtual-node --context kind-cluster-local -o custom-columns=NAME:.metadata.name --no-headers 2>/dev/null)" ]; do
  sleep 2
done

echo "\033[1;32mVirtual node detected! Waiting for it to become Ready...\033[0m"
kubectl wait --for=condition=Ready node -l liqo.io/type=virtual-node --timeout=120s --context kind-cluster-local

echo "\033[1;32mSetup complete! Virtual node is ready. NO namespaces are offloaded.\033[0m"
kubectl get nodes --context kind-cluster-local
