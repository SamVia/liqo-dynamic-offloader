#!/bin/bash
set -euo pipefail

echo -e "\033[1;36m=== In-Cluster Operator Demo (Helm Deployment) ===\033[0m"

# 1. Valutazione del parametro --dry-run
DRY_RUN="false"
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN="true"
  echo -e "\033[1;33m>>> DRY-RUN MODE ENABLED <<<\033[0m"
fi

TARGET_CLUSTERS="${TARGET_CLUSTER_ID:-cluster-remote}"
DEMO_REMOTE_CLUSTER="${DEMO_REMOTE_CLUSTER:-cluster-remote}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/samvia/liqo-dynamic-offloader}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-Always}"
HELM_RELEASE="${HELM_RELEASE:-liqo-dynamic-offloader}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-default}"

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "1. Deploying the Operator with Helm..."
# Remove the legacy hand-written deployment if a previous demo created it.
kubectl delete deployment liqo-auto-healing-operator \
  --namespace "$OPERATOR_NAMESPACE" \
  --ignore-not-found=true >/dev/null 2>&1
helm upgrade --install "$HELM_RELEASE" charts/liqo-dynamic-offloader \
  --namespace "$OPERATOR_NAMESPACE" \
  --create-namespace \
  --set-string image.repository="$IMAGE_REPOSITORY" \
  --set-string image.tag="$IMAGE_TAG" \
  --set image.pullPolicy="$IMAGE_PULL_POLICY" \
  --set-string config.targetClusterIDs="$TARGET_CLUSTERS" \
  --set-string config.excludedNamespaces="*-system,local-path-storage" \
  --set-string config.trapBlacklistLabels="dynamic-offloader.liqo.io/ignore-trap=true" \
  --set config.trapBackoff="5s" \
  --set config.cleanupDelay="15s" \
  --set config.dryRun="$DRY_RUN"

echo "Waiting for the operator to become ready..."
kubectl wait --for=condition=available "deployment/${HELM_RELEASE}" \
  --namespace "$OPERATOR_NAMESPACE" \
  --timeout=60s

echo -e "\n2. Preparing Test Namespaces..."
# A) Namespace standard
kubectl create namespace demo-allowed --dry-run=client -o yaml | kubectl apply -f -

# B) Namespace protetto dal Glob (*-system)
kubectl create namespace demo-system --dry-run=client -o yaml | kubectl apply -f -

# C) Namespace protetto da Label specifica
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: demo-labeled
  labels:
    dynamic-offloader.liqo.io/ignore-trap: "true"
EOF

# Pulizia di sicurezza prima del test
for ns in demo-allowed demo-system demo-labeled; do
  kubectl delete deployment trap-deployment -n $ns --ignore-not-found=true >/dev/null 2>&1
  kubectl delete namespaceoffloading offloading -n $ns --ignore-not-found=true >/dev/null 2>&1
done
sleep 2

cleanup() {
  echo -e "\nDemo terminated. Cleaning up..."
  kubectl delete namespace demo-allowed demo-system demo-labeled --ignore-not-found=true >/dev/null 2>&1
  helm uninstall "$HELM_RELEASE" --namespace "$OPERATOR_NAMESPACE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo -e "\n3. Springing the traps across all namespaces..."
for ns in demo-allowed demo-system demo-labeled; do
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trap-deployment
  namespace: $ns
spec:
  replicas: 1
  selector:
    matchLabels:
      app: trap
  template:
    metadata:
      labels:
        app: trap
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
      nodeSelector:
        kubernetes.io/hostname: ${DEMO_REMOTE_CLUSTER}
      tolerations:
      - key: "virtual-node.liqo.io/not-allowed"
        operator: "Exists"
EOF
done

echo -e "\nWaiting 10 seconds to allow the operator to process events..."
sleep 10

echo -e "\n\033[1;36m=== Evaluation Results ===\033[0m"
for ns in demo-allowed demo-system demo-labeled; do
  if kubectl get namespaceoffloading offloading -n "$ns" >/dev/null 2>&1; then
    echo -e "[\033[1;32mYES\033[0m] $ns -> Offloading Policy Created"
  else
    echo -e "[\033[1;31mNO\033[0m]  $ns -> No Policy (Filtered or Dry-Run)"
  fi
done

echo -e "\n---------------------------------------------------------------------"
echo "Streaming Operator Logs (Press CTRL+C to exit and cleanup)..."
echo "---------------------------------------------------------------------"
kubectl logs -f "deployment/${HELM_RELEASE}" --namespace "$OPERATOR_NAMESPACE"