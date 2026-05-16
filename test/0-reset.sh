#!/bin/bash

echo "Resetting Liqo Demo Environment..."

# Ensure we are operating on the local cluster
kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "1/2 Deleting the Trap Pod..."

kubectl delete pod liqo-trap-pod -n default --ignore-not-found=true --wait=true

echo "2/2 Stripping away NamespaceOffloading..."
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true --wait=true


echo -e "\033[1;32m\nStage Reset Complete!\033[0m"
echo "The default namespace is now strictly isolated."
echo "You are ready to run 2-demo.sh and start the Go Controller."