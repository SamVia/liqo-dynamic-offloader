#!/bin/bash
set -e

echo -e "\033[1;36m=== In-Cluster Operator Demo ===\033[0m"

TARGET_CLUSTERS="${TARGET_CLUSTER_ID:-cluster-remote}"
DEMO_REMOTE_CLUSTER="${DEMO_REMOTE_CLUSTER:-cluster-remote}"
DEMO_EXCLUDED_NAMESPACE="${DEMO_EXCLUDED_NAMESPACE:-demo-not-selected}"
EXCLUDED_NAMESPACES="${EXCLUDED_NAMESPACES:-kube-system,liqo-system,local-path-storage,crownlabs-system},${DEMO_EXCLUDED_NAMESPACE}"

echo "Allowed target clusters: ${TARGET_CLUSTERS:-all available remote clusters}"
echo "Demo virtual node: ${DEMO_REMOTE_CLUSTER}"

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "1. Skipping local image load (Configured to pull from GHCR)..."
# kind load docker-image liqo-dynamic-offloader:latest --name cluster-local

echo "2. Deploying the Operator (RBAC + Deployment)..."
kubectl apply -f config/rbac/role.yaml
kubectl delete deployment liqo-auto-healing-operator -n default --ignore-not-found=true >/dev/null 2>&1
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: liqo-auto-healing-sa
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: liqo-auto-healing-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: manager-role
subjects:
- kind: ServiceAccount
  name: liqo-auto-healing-sa
  namespace: default
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liqo-auto-healing-operator
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      control-plane: controller-manager
  template:
    metadata:
      labels:
        control-plane: controller-manager
    spec:
      serviceAccountName: liqo-auto-healing-sa
      containers:
      - name: manager
        # Point directly to the GHCR public registry
        image: ghcr.io/samvia/liqo-dynamic-offloader:latest
        # Force Kubernetes to download it instead of using local cache
        imagePullPolicy: Always
        env:
        - name: TARGET_CLUSTER_ID
          value: "${TARGET_CLUSTERS}"
        - name: EXCLUDED_NAMESPACES
          value: "${EXCLUDED_NAMESPACES}"
        - name: TRAP_BACKOFF
          value: "10s"
        - name: CLEANUP_GRACE_PERIOD
          value: "10s"
EOF

echo "Waiting for the operator pod to become ready..."
kubectl wait --for=condition=available deployment/liqo-auto-healing-operator -n default --timeout=60s

echo "3. Generating the Deep Trap Deployment YAML..."
cat <<EOF > /tmp/trap-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liqo-trap-deployment
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: liqo-trap
  template:
    metadata:
      labels:
        app: liqo-trap
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

cat <<EOF > /tmp/non-selected-deployment.yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ${DEMO_EXCLUDED_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liqo-non-selected-deployment
  namespace: ${DEMO_EXCLUDED_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: liqo-non-selected
  template:
    metadata:
      labels:
        app: liqo-non-selected
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

# clean leftovers
kubectl delete deployment liqo-trap-deployment -n default --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespace "${DEMO_EXCLUDED_NAMESPACE}" --ignore-not-found=true >/dev/null 2>&1
sleep 2

trap 'echo -e "\nDemo terminated. Cleaning up..."; kubectl delete deployment liqo-trap-deployment -n default --ignore-not-found=true >/dev/null 2>&1; kubectl delete deployment liqo-non-selected-deployment -n "${DEMO_EXCLUDED_NAMESPACE}" --ignore-not-found=true >/dev/null 2>&1; kubectl delete namespace "${DEMO_EXCLUDED_NAMESPACE}" --ignore-not-found=true >/dev/null 2>&1; kubectl delete deployment liqo-auto-healing-operator -n default --ignore-not-found=true >/dev/null 2>&1; kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true >/dev/null 2>&1; exit' SIGINT SIGTERM

echo -e "\n\033[1;32mOperator is running! Springing the trap...\033[0m"
kubectl apply -f /tmp/trap-deployment.yaml
kubectl apply -f /tmp/non-selected-deployment.yaml

echo "Checking that the excluded namespace is not offloaded..."
sleep 5
if kubectl get namespaceoffloading offloading -n "${DEMO_EXCLUDED_NAMESPACE}" >/dev/null 2>&1; then
  echo "ERROR: an offloading policy was created for ${DEMO_EXCLUDED_NAMESPACE}"
  exit 1
fi
echo "OK: ${DEMO_EXCLUDED_NAMESPACE} has no NamespaceOffloading policy."

echo "---------------------------------------------------------------------"
echo "Streaming Operator Logs (Press CTRL+C to exit)..."
echo "---------------------------------------------------------------------"

# Shows operator logs in real time
kubectl logs -f deployment/liqo-auto-healing-operator -n default