# K8s Docker Operator: Test Case Scenarios

This document outlines the comprehensive test scenarios for verifying the functionality, stability, and performance of the Kubernetes Docker Operator.

## Prerequisites
- **Kubernetes Cluster**: Kind (Kubernetes in Docker) or any standard K8s cluster.
- **Docker Host**: Access to the Docker Daemon where the operator is running (for verifying external containers).
- **Operator Version**: >= `v0.0.89` (Required for Sub-Second Reload & Race Condition Protection).

---

## 1. Functional Verification: Basic Lifecycle

### Scenario 1.1: Deploy DockerDeployment (Single Replica)
**Steps:**
1. Apply manifest: `kubectl apply -f examples/dockerdeployment-basic.yaml`
2. Verify CR status: `kubectl get dockerdeployment nginx-app` (Expect: Available/Ready)
3. Verify external container: `docker ps | grep nginx-app` (Expect: 1 container running)

### Scenario 1.2: Deploy DockerService (Tunnel Establishment)
**Steps:**
1. Apply manifest: `kubectl apply -f examples/dockerservice-basic.yaml`
2. Verify Tunnel Server Pod: `kubectl get pods -l app=tunnel-nginx-app` (Expect: Running)
3. Verify Tunnel Client Container: `docker ps | grep tunnel-client-nginx-app` (Expect: 1 container running)
4. Verify Connectivity:
   ```bash
   kubectl port-forward svc/tunnel-nginx-app 8080:8080 &
   curl -I localhost:8080
   ```
   *(Expect: HTTP 200 OK from Nginx)*

---

## 2. Scaling & Load Balancing

### Scenario 2.1: Scale Up DockerDeployment
**Steps:**
1. Update manifest `examples/dockerdeployment-scale.yaml` to `replicas: 3`.
2. Apply: `kubectl apply -f examples/dockerdeployment-scale.yaml`
3. Verify external containers: `docker ps | grep nginx-scaled` (Expect: 3 containers running)
4. Verify Tunnel Client: `docker ps | grep tunnel-client-nginx-lb` (Expect: **STILL 1** container - Single Tunnel Client Architecture)

### Scenario 2.2: Load Balancer Traffic Distribution
**Steps:**
1. Apply Service: `kubectl apply -f examples/dockerservice-loadbalanced.yaml`
2. Port-forward Tunnel Service: `kubectl port-forward svc/tunnel-nginx-lb 8080:8080`
3. Generate traffic loop:
   ```bash
   for i in {1..10}; do curl -s localhost:8080 | grep "Hostname"; done
   ```
   *(Expect: Responses from different Hostnames/IPs indicating Round-Robin load balancing)*

---

## 3. Stability & Performance (v0.0.89+)

### Scenario 3.1: Sub-Second Push Reload (Latency Test)
**Objective:** Verify that backend scaling updates propagate to the Tunnel Server in < 1 second.
**Steps:**
1. Tail Tunnel Server logs: `kubectl logs -f -l app=tunnel-nginx-lb`
2. In another terminal, scale deployment: `kubectl scale dockerdeployment nginx-scaled --replicas=5`
3. Watch logs for: `Targets reloaded via Push: [...]`
   *(Expect: Log appears almost instantly (<500ms) after the Scaling event.*

### Scenario 3.2: Race Condition Protection (Push Priority)
**Objective:** Confirm that slow ConfigMap file syncs do not revert fresh Push updates.
**Steps:**
1. Tail Tunnel Server logs.
2. Trigger a Push update (via Scaling or Annotation).
3. Observe: `Targets reloaded via Push`.
4. Wait ~5-10 seconds for Kubelet to sync the ConfigMap file.
5. Observe: **NO** `Dynamic targets updated from file` log entry (or log indicates it was ignored due to priority/staleness).
   *(Expect: The Push remains the source of truth for at least 2 minutes.)*

### Scenario 3.3: Zero-Interruption Stability (Connection Persistence)
**Objective:** Ensure active connections are not dropped during scaling.
**Steps:**
1. Start a continuous connection test loop (e.g., `while true; do curl ...; sleep 0.1; done`).
2. Scale deployment up (3 -> 5) and down (5 -> 1) aggressively.
3. Monitor output for errors (502 Bad Gateway, Connection Refused).
   *(Expect: **Zero** errors. Traffic may pause briefly but should not fail.)*

---

## 4. DockerJob Execution

### Scenario 4.1: Run Default DockerJob
**Steps:**
1. Apply manifest: `kubectl apply -f examples/dockerjob-basic.yaml`
2. Verify Job Status: `kubectl get dockerjob hello-world` (Expect: Succeeded)
3. Check Logs: Find pod or external container logs to verify output "Hello from DockerJob!".

### Scenario 4.2: DockerJob with Advanced Config (Env/Resources)
**Steps:**
1. Apply manifest: `kubectl apply -f examples/dockerjob-advanced.yaml`
   *(Ensure prerequisite secrets/host match config)*
2. Verify Resource Limits: Inspect created container via `docker inspect`.
3. Verify Environment Variables: `docker exec <container_id> env`.

---

## 5. Multi-Host Connectivity (DockerHost)

### Scenario 5.1: Connect to Remote Docker Host
**Steps:**
1. Apply manifest: `examples/dockerhost-remote.yaml` (Update spec with valid connection details).
2. Create DockerContainer referencing `dockerHostRef: remote-host`.
3. Verify Container: Check `docker ps` on the **remote** machine.
   *(Expect: Container running on the remote host, not local.)*

---

## 6. Cleanup & Garbage Collection

### Scenario 6.1: Delete DockerService
**Steps:**
1. Delete: `kubectl delete -f examples/dockerservice-loadbalanced.yaml`
2. Verify Tunnel Server Pod: `kubectl get pods` (Expect: Terminating/Gone)
3. Verify Tunnel Client Container: `docker ps` (Expect: `tunnel-client-nginx-lb` is removed)

### Scenario 6.2: Delete DockerDeployment
**Steps:**
1. Delete: `kubectl delete -f examples/dockerdeployment-scale.yaml`
2. Verify Backend Containers: `docker ps` (Expect: All `nginx-scaled-*` containers are removed)

---

## 7. Storage & Configuration (Secrets/Volumes)

### Scenario 7.1: Host Volume Mounting
**Steps:**
1. Apply manifest with `volumeMounts`: `kubectl apply -f examples/dockercontainer-volume.yaml`
2. Verify mount: `docker inspect <container_id> --format '{{ .Mounts }}'`
   *(Expect: Source host path correctly mapped to destination path inside container)*
3. Write file on host, check inside container: `docker exec <container_id> cat /data/test.txt`.

### Scenario 7.2: K8s Secret as File Volume
**Steps:**
1. Create Secret: `kubectl create secret generic app-config --from-literal=config.json='{"key": "value"}'`
2. Apply `DockerContainer` referencing secret in `secretVolumes`.
3. Verify: `docker exec <container_id> cat /etc/config/config.json`
   *(Expect: Secret content matches Kubernetes data)*

### Scenario 7.3: K8s Secret as Environment Variable
**Steps:**
1. Create Secret: `kubectl create secret generic app-env --from-literal=API_KEY=secret123`
2. Apply `DockerContainer` with `envVars` using `valueFrom.secretKeyRef`.
3. Verify: `docker exec <container_id> env | grep API_KEY`
   *(Expect: `API_KEY=secret123`)*

---

## 8. Advanced Networking & Security

### Scenario 8.1: mTLS Remote Connectivity
**Steps:**
1. Setup remote Docker host with mTLS (see [docker-mtls-setup.md](docker-mtls-setup.md)).
2. Create K8s Secret with certs: `kubectl create secret generic remote-docker-certs --from-file=ca.pem --from-file=cert.pem --from-file=key.pem`
3. Define `DockerHost` with `tlsSecretName: remote-docker-certs`.
4. Deploy container to this host.
   *(Expect: Successful connection and container creation without certificate errors)*

### Scenario 8.2: Private Image Registry (ImagePullSecret)
**Steps:**
1. Create `docker-registry` secret in K8s.
2. Apply `DockerContainer` with `imagePullSecret: my-registry-key` and a private image.
3. Verify: Container successfully pulls and runs.

---

## 9. Resilience & Disaster Recovery

### Scenario 9.1: Docker Daemon Restart
**Steps:**
1. Ensure containers are running via operator.
2. Restart Docker service on host: `sudo systemctl restart docker`.
3. Observe Operator behavior.
   *(Expect: Operator detects containers are gone (if non-persistent) or re-attaches and reconciles state to match K8s spec)*

### Scenario 9.2: Tunnel Server Pod Failure
**Steps:**
1. Delete the Tunnel Server Pod: `kubectl delete pod -l app=tunnel-nginx-app`
2. Monitor service availability.
   *(Expect: K8s recreates the Pod; Tunnel Client re-connects automatically; Traffic resumes in < 5 seconds)*

### Scenario 9.3: Operator Restart
**Steps:**
1. Delete the operator manager pod.
2. While operator is down, manually delete a Docker container: `docker rm -f <container_id>`.
3. Start operator.
   *(Expect: Operator performs a full re-sync and recreates the missing container)*

### Scenario 9.4: Tunnel Client Container Failure
**Steps:**
1. Identify the tunnel client container on the Docker host: `docker ps | grep tunnel-client`.
2. Kill it: `docker rm -f <tunnel-client-id>`.
3. Verify connectivity.
   *(Expect: Operator detects deletion and recreates it; Tunnel is restored automatically within seconds)*

---

## 10. Update & Lifecycle Management

### Scenario 10.1: Rolling Image Update
**Steps:**
1. Update `image` tag in a `DockerDeployment` manifest.
2. Apply update.
3. Watch `docker ps`.
   *(Expect: New containers created with updated image; Old containers removed only AFTER new ones are healthy)*

### Scenario 10.2: Configuration Hot-Swap
**Steps:**
1. Update `env` or `labels` in `DockerContainer` spec.
2. Observe container.
   *(Expect: Operator restarts container with new settings or applies them if supported)*

---

## 11. Advanced Job Management

### Scenario 11.1: Job Backoff Limit (Failure Retries)
**Steps:**
1. Apply `DockerJob` with a failing command (e.g., `exit 1`) and `backoffLimit: 3`.
2. Monitor job status.
   *(Expect: Job restarts 3 times before being marked as Failed)*

### Scenario 11.2: Active Deadline (Timeout)
**Steps:**
1. Apply `DockerJob` with `sleep 100` and `activeDeadlineSeconds: 10`.
2. Verify: Job is terminated and status is `Failed` (Reason: DeadlineExceeded) after 10s.

### Scenario 11.3: TTL After Finished
**Steps:**
1. Apply `DockerJob` with `ttlSecondsAfterFinished: 60`.
2. Wait for job to succeed.
3. Wait > 60 seconds.
---

## 12. Edge Cases & Conflict Resolution

### Scenario 12.1: Identity Conflict (Name Collision)
**Steps:**
1. Create two `DockerContainer` objects in different namespaces but with the same `spec.containerName` pointing to the same `DockerHost`.
2. Observe status.
   *(Expect: One succeeds, the other shows an Error/Warning in events regarding name conflict)*

### Scenario 12.2: External Interference
**Steps:**
1. Manually stop a container managed by the operator: `docker stop <container_id>`.
2. Wait for reconciliation period.
   *(Expect: Operator detects the `Exited` status and restarts it to maintain `Running` state)*

### Scenario 12.3: Native Docker Health Checks
**Steps:**
1. Apply `DockerContainer` with `healthCheck`:
   ```yaml
   healthCheck:
     test: ["CMD", "curl", "-f", "http://localhost/health"]
     interval: 5s
     retries: 3
   ```
---

## 13. Logging & Monitoring

### Scenario 13.1: Log Streaming Verification
**Objective:** Confirm container logs are accessible.
**Steps:**
1. Run a container that outputs logs continuously (e.g., `while true; do echo "tick"; sleep 1; done`).
2. Verify via Docker: `docker logs -f <container_name>`.
3. Verify via Operator (if exposed): `curl -i "http://<operator-pod-ip>:8080/logs?name=<container_name>&tail=10"`.
   *(Expect: Logs are streamed correctly from the Docker engine to the requestor)*
