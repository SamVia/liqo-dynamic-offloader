#!/bin/bash
set -euo pipefail

echo "Resetting Liqo Demo Environment..."

# Ensure we are operating on the local cluster
kubectl config use-context kind-cluster-local >/dev/null 2>&1
HELM_RELEASE="${HELM_RELEASE:-liqo-dynamic-offloader}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-default}"

echo "1/4 Deleting the Trap Deployment..."
kubectl delete deployment liqo-trap-deployment -n default --ignore-not-found=true --wait=true

echo "2/4 Stripping away NamespaceOffloading..."
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true --wait=true

echo "3/4 Removing Demo Operator..."
helm uninstall "$HELM_RELEASE" --namespace "$OPERATOR_NAMESPACE" >/dev/null 2>&1 || true
kubectl delete deployment liqo-auto-healing-operator \
  --namespace "$OPERATOR_NAMESPACE" \
  --ignore-not-found=true --wait=true

echo "4/4 Cleaning up Excluded Namespaces..."
kubectl delete namespace demo-not-selected demo-allowed demo-system demo-labeled \
  --ignore-not-found=true --wait=true

echo -e "\033[1;32m\nStage Reset Complete!\033[0m"
echo "The default namespace is now strictly isolated and the operator has been removed."
echo "You are ready to start a fresh demo."