#!/bin/bash
set -euo pipefail

echo -e "\033[1;36m=== In-Cluster Operator Demo (Helm Deployment) ===\033[0m"
bash ./scripts/1-setup.sh 1
# 1. Evaluate the --dry-run parameter.
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

HELM_TARGET_CLUSTERS="${TARGET_CLUSTERS//,/\\,}"

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "1. Deploying the Operator with Helm..."
kubectl delete deployment liqo-auto-healing-operator \
  --namespace "$OPERATOR_NAMESPACE" \
  --ignore-not-found=true >/dev/null 2>&1
helm upgrade --install "$HELM_RELEASE" charts/liqo-dynamic-offloader \
  --namespace "$OPERATOR_NAMESPACE" \
  --create-namespace \
  --set-string image.repository="$IMAGE_REPOSITORY" \
  --set-string image.tag="$IMAGE_TAG" \
  --set image.pullPolicy="$IMAGE_PULL_POLICY" \
  --set-string config.targetClusterIDs="$HELM_TARGET_CLUSTERS" \
  --set-string 'config.excludedNamespaces=*-system\,local-path-storage' \
  --set-string config.trapBlacklistLabels="dynamic-offloader.liqo.io/ignore-trap=true" \
  --set config.trapBackoff="5s" \
  --set config.cleanupDelay="15s" \
  --set config.dryRun="$DRY_RUN" >/dev/null

echo "Waiting for the operator to become ready..."
kubectl wait --for=condition=available "deployment/${HELM_RELEASE}" \
  --namespace "$OPERATOR_NAMESPACE" \
  --timeout=60s >/dev/null

echo -e "\n2. Preparing Test Namespaces..."
kubectl create namespace demo-allowed --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create namespace demo-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: demo-labeled
  labels:
    dynamic-offloader.liqo.io/ignore-trap: "true"
EOF

# Perform safety cleanup before the test.
for ns in demo-allowed demo-system demo-labeled; do
  kubectl delete deployment trap-deployment -n $ns --ignore-not-found=true >/dev/null 2>&1
  kubectl delete namespaceoffloading offloading -n $ns --ignore-not-found=true >/dev/null 2>&1
done
sleep 2

cleanup() {
  echo -e "\nDemo terminated. Cleaning up..."
  kill "${PF_PID:-}" 2>/dev/null || true
  kill "${WATCH_PID:-}" 2>/dev/null || true
  kubectl delete namespace demo-allowed demo-system demo-labeled --ignore-not-found=true >/dev/null 2>&1
  helm uninstall "$HELM_RELEASE" --namespace "$OPERATOR_NAMESPACE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo -e "\n\033[1;33m>>> Starting Pod Watcher in 'demo-allowed' <<<\033[0m"
# Show state changes in real time during the demo.
kubectl get pods -n demo-allowed -w &
WATCH_PID=$!

echo -e "\n3. PHASE 1: Springing the traps across all namespaces..."
for ns in demo-allowed demo-system demo-labeled; do
cat <<EOF | kubectl apply -f - >/dev/null
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

echo -e "\nWaiting 15 seconds to allow the operator to process events and Liqo to schedule..."
sleep 15

echo -e "\n\033[1;36m=== Phase 1 Results (Trap Creation) ===\033[0m"
for ns in demo-allowed demo-system demo-labeled; do
  if kubectl get namespaceoffloading offloading -n "$ns" >/dev/null 2>&1; then
    echo -e "[\033[1;32mYES\033[0m] $ns -> Offloading Policy Created"
  else
    echo -e "[\033[1;31mNO\033[0m]  $ns -> No Policy (Filtered or Dry-Run)"
  fi
done

echo -e "\n4. PHASE 2: Testing Cleanup Deletion..."
if [[ "$DRY_RUN" == "false" ]]; then
  echo "Deleting the deployment from 'demo-allowed' to trigger cleanup countdown..."
  kubectl delete deployment trap-deployment -n demo-allowed >/dev/null 2>&1
  
  echo "Waiting 30 seconds for the 15s timer to expire..."
  for i in {1..10}; do
    if ! kubectl get namespaceoffloading offloading -n demo-allowed >/dev/null 2>&1; then
      break
    fi
    sleep 3
  done
  
  echo -e "\n\033[1;36m=== Phase 2 Results (Cleanup Deletion) ===\033[0m"
  if kubectl get namespaceoffloading offloading -n demo-allowed >/dev/null 2>&1; then
    echo -e "[\033[1;31mFAIL\033[0m] demo-allowed -> Policy is STILL THERE (Cleanup failed!)"
  else
    echo -e "[\033[1;32mSUCCESS\033[0m] demo-allowed -> Policy was successfully DELETED."
  fi
else
  echo "Skipping Phase 2 because Dry-Run is enabled."
fi

# Stop the pod watch to clean up the final output.
kill "${WATCH_PID:-}" 2>/dev/null || true

echo -e "\n\033[1;36m=== Operator Prometheus Metrics ===\033[0m"
kubectl port-forward -n "$OPERATOR_NAMESPACE" "deployment/${HELM_RELEASE}" 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
sleep 2
if curl -s --max-time 2 http://localhost:18080/metrics >/dev/null 2>&1; then
  curl -s http://localhost:18080/metrics | grep -E '^liqo_' || true
fi
kill "$PF_PID" 2>/dev/null || true

echo -e "\n---------------------------------------------------------------------"
echo "Streaming Operator Logs (Press CTRL+C to exit and cleanup)..."
echo "---------------------------------------------------------------------"
kubectl logs -f "deployment/${HELM_RELEASE}" --namespace "$OPERATOR_NAMESPACE"