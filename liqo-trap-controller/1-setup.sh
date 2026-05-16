!/bin/bash
set -e

echo "Step 1: Creating local and remote Kind clusters..."
kind create cluster --name cluster-local || true
kind create cluster --name cluster-remote || true

echo "Step 2: Installing Liqo (v1.0+)..."
kubectl config use-context kind-cluster-local
liqoctl install kind --cluster-id cluster-local

kubectl config use-context kind-cluster-remote
liqoctl install kind --cluster-id cluster-remote

echo "Step 3: Peering clusters using NodePort..."
liqoctl peer --context kind-cluster-local --remote-context kind-cluster-remote --gw-server-service-type NodePort

echo "Waiting for Liqo virtual node to be created by the controller..."
while [ -z "$(kubectl get nodes -l liqo.io/type=virtual-node --context kind-cluster-local -o custom-columns=NAME:.metadata.name --no-headers 2>/dev/null)" ]; do
  sleep 2
done

echo "Virtual node detected! Waiting for it to become Ready..."
kubectl wait --for=condition=Ready node -l liqo.io/type=virtual-node --timeout=120s --context kind-cluster-local

echo "Setup complete! Virtual node is ready. NO namespaces are offloaded."
kubectl get nodes --context kind-cluster-local