# Kubernetes Docker Operator

A Kubernetes operator designed to manage Docker containers across multiple Docker hosts directly from Kubernetes. It bridge the gap between Docker and Kubernetes by providing seamless lifecycle management and automated network tunneling.

## 🚀 Features

| Feature | Description |
| :--- | :--- |
| **Multi-Host Support** | Connect to multiple Docker daemons via Unix socket or TCP with TLS. |
| **Lifecycle Management** | Control container state (Image, Command, Env, Restart) via Kubernetes CRDs. |
| **Job Runs** | Run one-off tasks with exit code tracking, retries, timeout, and TTL cleanup. |
| **Secret Integration** | Map Kubernetes Secrets to container environment variables or files. |
| **Automated Tunneling** | Expose Docker ports as Kubernetes Services using WebSocket-based reverse tunnels. |
| **Tunnel Gateway** | Centralized NodePort entry point for all tunnels, keeping app services internal. |
| **Private Registry Auth** | Use standard Kubernetes `ImagePullSecrets` for private image pulls. |
| **Volume Management** | Easily bind host paths to containers for persistent data or configuration. |
| **Kind Compatibility** | Specialized network modes for seamless integration with local Kind clusters. |

## Why Use This? (The Abstraction Philosophy)

This operator provides a **high-level abstraction** for managing Docker workloads, bridging the gap between Kubernetes power and lightweight execution.

### 1. "Serverless-like" Experience on Your Own Metal
Turn any machine with Docker installed into a managed compute node.
*   **No OS Management**: The operator treats the host as a dumb "Docker runner".
*   **No K8s Overhead**: Avoid the 500MB+ RAM tax of Kubelet/Kube-Proxy. Perfect for `t3.micro` instances or Raspberry Pis.
*   **GitOps Everything**: Define your legacy/edge Docker apps in YAML, manage them with ArgoCD, just like your cluster apps.

### 2. The "Remote Docker" Pattern
Instead of joining nodes to the cluster (heavy, secure VPNs needed), we use a **reverse tunnel**.
*   **Firewall Friendly**: Works behind NAT, corporate firewalls, and 4G networks.
*   **Zero-Trust**: The agent dials OUT to the cluster. No inbound ports required on the edge device.

### 3. Cost-Efficiency at Scale
*   **100% Resource Utilization**: Run microservices on $3/mo instances without losing 50% RAM to system overhead.
*   **Spot Instance Ready**: If a node dies, the CRD reconciliation loop ensures state converges when it returns.

## 📦 Installation

For detailed setup instructions, including our **Automated Quickstart Script**, please see the [Quickstart Guide](docs/quickstart.md).

Quick summary:
```bash
# Automated Setup (Recommended)
./scripts/kind-quickstart.sh

# Manual Setup
kubectl apply -f install/install.yaml
```

## 🛠 Usage

You can find a variety of example configurations in the [examples/](examples/) directory.

### 1. Define a Docker Host (Remote)

If you want to use a remote Docker daemon, create a `DockerHost` resource:

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerHost
metadata:
  name: remote-host
spec:
  hostURL: "tcp://1.2.3.4:2376"
  tlsSecretName: docker-tls-secret # Optional: Secret containing ca.pem, cert.pem, key.pem
```

### 2. Deploy a Container with Tunneling

Deploy an Nginx container on a specific host and expose it to the Kubernetes cluster:

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerContainer
metadata:
  name: nginx-basic
spec:
  image: "nginx:latest"
  containerName: "my-nginx"
  dockerHostRef: "remote-host" # Omit for local Docker socket
  services:
    - name: http
      port: 80
      targetPort: 80
```

Once applied, the operator will:
1.  Pull the image on the target Docker host.
2.  Start the container.
3.  Deploy a **Tunnel Server** pod in Kubernetes.
4.  Start a **Tunnel Client** alongside your container on the Docker host.
5.  Create a **Kubernetes Service** named `tunnel-nginx-basic` pointing to the container.

### 3. Volume Mounting

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerContainer
metadata:
  name: nginx-volume
spec:
  image: "nginx:latest"
  volumeMounts:
    - hostPath: "/tmp/data"
      containerPath: "/usr/share/nginx/html"
      readOnly: true
```

### 4. Kubernetes Secret Mapping

Inject sensitive data as environment variables or files:

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerContainer
metadata:
  name: nginx-secret
spec:
  image: "nginx:latest"
  containerName: "nginx-with-secrets"
  # Map Secret keys to Environment Variables
  envVars:
    - name: DB_PASSWORD
      valueFrom:
        secretKeyRef:
          name: my-test-secret
          key: password
  # Map entire Secret to a directory
  secretVolumes:
    - secretName: my-test-secret
      mountPath: "/etc/secrets"
```

### 5. Running a One-Off Job

Run a one-off task (like a database migration or batch script) that tracks completion:

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerJob
metadata:
  name: db-migrate
spec:
  image: "my-app:latest"
  command: ["./migrate", "--direction", "up"]
  dockerHostRef: "remote-host"
  restartPolicy: Never
  backoffLimit: 3
  activeDeadlineSeconds: 300
  ttlSecondsAfterFinished: 600
  envVars:
    - name: DATABASE_URL
      valueFrom:
        secretKeyRef:
          name: db-credentials
          key: connection-string
```

The operator will:
1.  Pull the image and start the container.
2.  Monitor until the container exits.
3.  Report `Succeeded` (exit 0) or `Failed` (non-zero) in `status.phase`.
4.  Retry up to `backoffLimit` times on failure.
5.  Clean up the container after `ttlSecondsAfterFinished`.

## 🏗 Architecture (Tunnel Gateway)

The operator uses a centralized **Tunnel Gateway** architecture to minimize external port exposure:

-   **Tunnel Gateway**: Listens on a fixed NodePort (`30000`). This is the only port exposed externally for tunneling.
-   **Tunnel Server**: Runs inside Kubernetes as a `ClusterIP` service. It remains private and secure.
-   **Tunnel Client**: Runs on the Docker host, connecting to the Gateway to establish a reverse tunnel back to Kubernetes.

## 📜 License

Copyright 2024. Licensed under the Apache License, Version 2.0.
