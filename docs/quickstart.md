# Quickstart Guide

This guide will help you get the **Kubernetes Docker Operator** up and running quickly.

## Prerequisites

Ensure you have the following tools installed:
*   [Docker](https://docs.docker.com/get-docker/)
*   [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) (Kubernetes in Docker)
*   [kubectl](https://kubernetes.io/docs/tasks/tools/)

## 🚀 Automated Setup (Recommended)

We provide a script to automate the entire setup process for a local development environment.

Run the quickstart script:

```bash
./scripts/kind-quickstart.sh
```

This script will:
1.  Build the operator Docker image.
2.  Create (or recreate) a Kind cluster with the necessary docker socket mount.
3.  Load the built image into the cluster.
4.  Install the Operator and the Docker Proxy.
5.  Wait for all components to be ready.

Once complete, you can try deploying an example container:

```bash
kubectl apply -f examples/dockercontainer-basic.yaml
```

## 🛠 Manual Setup

If you prefer to set things up step-by-step or are running on a different cluster, follow these instructions.

### 1. Cluster Configuration (Kind)

If using Kind, you **must** mount the Docker socket so the operator can access the host's Docker daemon.

Create a `kind-config.yaml`:
```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /var/run/docker.sock
    containerPath: /var/run/docker.sock
```

Create the cluster:
```bash
kind create cluster --config kind-config.yaml
```

### 2. Install the Operator

Apply the core installation manifest. This includes CRDs, RBAC roles, and the Controller Manager deployment.

```bash
kubectl apply -f install/install.yaml
```

### 3. Install Docker Proxy (Optional / Kind-Specific)

For Kind clusters, we use a proxy to securely connect to the host's Docker socket without exposing it to every pod.

```bash
kubectl apply -f install/kind-setup.yaml
```

This deploys:
*   **Docker Proxy**: A minimal StatefulSet that bridges the mounted socket.
*   **Default DockerHost**: A `DockerHost` Custom Resource in the `system` namespace, configured to use this proxy.

### 4. Verification

Check that the pods are running:

```bash
kubectl -n system get pods
```

You should see:
*   `controller-manager-xxx`
*   `tunnel-gateway-xxx`
*   `docker-proxy-0`

## Next Steps

*   Explore [Examples](../examples/) for more usage patterns.
*   Read about [Docker Jobs](jobs.md).
