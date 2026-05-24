#!/bin/bash

echo "Resetting Liqo Demo Environment..."

# Ensure we are operating on the local cluster
kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "1/3 Deleting resources from test-liqo..."
# Deleting the namespace automatically deletes the 'test-pod' inside it
kubectl delete namespace test-liqo --ignore-not-found=true --wait=true

echo "2/3 Deleting legacy default Trap Pod (if any)..."
kubectl delete pod liqo-trap-pod -n default --ignore-not-found=true --wait=true

echo "3/3 Stripping away NamespaceOffloading..."
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true --wait=true


echo -e "\033[1;32m\nStage Reset Complete!\033[0m"
echo "The test-liqo resources have been completely wiped."
echo "You are ready to run your setup or demo scripts again."