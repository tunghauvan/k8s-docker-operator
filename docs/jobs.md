# DockerJob: Running One-Off Tasks

The `DockerJob` resource allows you to run one-off tasks (batch jobs, migrations, scripts) on your Docker hosts, managed by Kubernetes.

## Overview

Unlike `DockerContainer`, which ensures a container is always running (like a Deployment/DaemonSet), `DockerJob` ensures a container runs to completion (like a Kubernetes Job).

Key features:
- **Completion Tracking**: Monitors exit codes. `0` = Succeeded, `non-zero` = Failed.
- **Retries**: Automatically restart failed containers based on `backoffLimit`.
- **Timeouts**: Terminate jobs that run longer than `activeDeadlineSeconds`.
- **Cleanup**: Automatically remove finished containers and their logs after `ttlSecondsAfterFinished`.

## specification

### Basic Example

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerJob
metadata:
  name: batch-process-1
spec:
  image: "python:3.9-slim"
  command: ["python", "process_data.py"]
  restartPolicy: Never
```

### Full Specification

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerJob
metadata:
  name: database-backup
spec:
  # 1. Image and Command
  image: "postgres:15-alpine"
  command: ["pg_dump", "-h", "db-host", "-U", "user", "dbname"]
  
  # 2. Host Selection
  dockerHostRef: "remote-worker-1" # Optional: defaults to local
  
  # 3. Failure Handling
  restartPolicy: OnFailure # "Never" or "OnFailure"
  backoffLimit: 4          # Number of retries before marking as Failed (default: 6)
  
  # 4. Timeouts & Cleanup
  activeDeadlineSeconds: 1800  # Max runtime in seconds (30 mins)
  ttlSecondsAfterFinished: 3600 # Delete container 1 hour after finish
  
  # 5. Environment & Secrets
  envVars:
    - name: PGPASSWORD
      valueFrom:
        secretKeyRef:
          name: db-secret
          key: password
          
  # 6. Volumes
  volumeMounts:
    - hostPath: "/mnt/backups"
      containerPath: "/backups"
```

## Lifecycle States

| Phase | Description |
|:---|:---|
| **Pending** | The controller is preparing to create the container (pulling image, etc). |
| **Running** | The container is currently running / executing. |
| **Succeeded** | The container exited with code `0`. |
| **Failed** | The container exited with non-zero code and exhausted retries, or hit the deadline. |

## Failure Handling

- If `restartPolicy: OnFailure`: The *same* container is restarted (or removed and recreated depending on Docker driver behavior).
- If `restartPolicy: Never`: A *new* container is created for each retry attempt, until `backoffLimit` is reached.

## Garbage Collection (TTL)

To prevent your Docker hosts from filling up with old stopped containers, use `ttlSecondsAfterFinished`.
- If set, the controller checks `status.completionTime`.
- Once `now > completionTime + ttl`, the Docker container is removed.
- The `DockerJob` CR in Kubernetes is **NOT** deleted, only the underlying Docker container.
