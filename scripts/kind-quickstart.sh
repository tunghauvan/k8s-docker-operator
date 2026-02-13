#!/bin/bash
set -e

# Configuration
CLUSTER_NAME="kind"
IMAGE_NAME="hvtung/k8s-docker-operator"
VERSION=$(cat VERSION)
FULL_IMAGE="${IMAGE_NAME}:${VERSION}"

echo "🚀 Starting Kind Quickstart for k8s-docker-operator (v${VERSION})..."

# 1. Build Docker Image
echo "📦 Building Docker image..."
make docker-build

# Re-read version after bump
VERSION=$(cat VERSION)
FULL_IMAGE="${IMAGE_NAME}:${VERSION}"

# 2. Check/Create Kind Cluster
if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  echo "✅ Cluster '${CLUSTER_NAME}' already exists. Skipping creation."
else
  echo "🚀 Creating Kind cluster..."
  kind create cluster --name ${CLUSTER_NAME} --config kind-config.yaml
fi

# 3. Load Image into Kind
echo "🚚 Loading image into Kind..."
kind load docker-image ${FULL_IMAGE} --name ${CLUSTER_NAME}

# 4. Generate & Install Manifests
echo "📄 Generating and applying manifests..."
make dist
kubectl apply -f install/install.yaml
kubectl apply -f install/kind-setup.yaml

# 5. Wait for Readiness
echo "⏳ Waiting for operator to be ready..."
kubectl wait --namespace system \
  --for=condition=ready pod \
  --selector=control-plane=controller-manager \
  --timeout=90s

kubectl wait --namespace system \
  --for=condition=ready pod \
  --selector=app=docker-proxy \
  --timeout=90s

echo "✅ Quickstart Complete! Operator is running."
echo "   Try creating a container:"
echo "   kubectl apply -f examples/dockercontainer-basic.yaml"
