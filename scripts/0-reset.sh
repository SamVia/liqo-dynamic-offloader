#!/bin/bash
set -euo pipefail

echo "Resetting Liqo Demo Environment..."

# Ensure we are operating on the local cluster
kubectl config use-context kind-cluster-local >/dev/null 2>&1
HELM_RELEASE="${HELM_RELEASE:-liqo-dynamic-offloader}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-default}"

echo "1/5 Terminating Local Operator Processes..."
pkill -9 -f "liqo-manager-test" 2>/dev/null || true
pkill -9 -f "exe/main" 2>/dev/null || true
pkill -9 -f "go run" 2>/dev/null || true

echo "2/5 Deleting the Trap Deployments..."
kubectl delete deployment liqo-trap-deployment -n default --ignore-not-found=true --wait=true
kubectl delete deployment trap-deployment -n default --ignore-not-found=true --wait=true

echo "3/5 Stripping away NamespaceOffloading..."
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true --wait=true

echo "4/5 Removing Demo Operator..."
helm uninstall "$HELM_RELEASE" --namespace "$OPERATOR_NAMESPACE" >/dev/null 2>&1 || true
kubectl delete deployment liqo-auto-healing-operator \
  --namespace "$OPERATOR_NAMESPACE" \
  --ignore-not-found=true --wait=true

echo "5/5 Cleaning up Test Namespaces..."
kubectl delete namespace demo-not-selected demo-allowed demo-system demo-labeled test-resilience test-edge-case test-cluster-alpha test-cluster-beta \
  --ignore-not-found=true --wait=true

echo -e "\033[1;32m\nStage Reset Complete!\033[0m"
echo "The cluster is clean, local ghosts are busted, and you are ready for a fresh demo."