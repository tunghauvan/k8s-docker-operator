package dockercontainer

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
)

// HandleLogs is an HTTP handler that streams Docker container logs.
// Query params: name (container name), follow (bool), tail (string, e.g. "100")
func (r *DockerContainerReconciler) HandleLogs(w http.ResponseWriter, req *http.Request) {
	containerName := req.URL.Query().Get("name")
	if containerName == "" {
		http.Error(w, "missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	follow := req.URL.Query().Get("follow") == "true"
	tail := req.URL.Query().Get("tail")
	if tail == "" {
		tail = "100"
	}

	// Use the default local Docker client for logs
	cli, err := r.getLogsClient()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create docker client: %v", err), http.StatusInternalServerError)
		return
	}

	ctx := req.Context()
	logsOpts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tail,
		Timestamps: true,
	}

	reader, err := cli.ContainerLogs(ctx, containerName, logsOpts)
	if err != nil {
		if dockerclient.IsErrNotFound(err) {
			http.Error(w, fmt.Sprintf("container '%s' not found", containerName), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to get logs: %v", err), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Transfer-Encoding", "chunked")

	// Stream logs to response
	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				f.Flush()
			}
			if readErr != nil {
				break
			}
		}
	} else {
		io.Copy(w, reader)
	}
}

// getLogsClient returns a Docker client for streaming logs.
func (r *DockerContainerReconciler) getLogsClient() (dockerclient.APIClient, error) {
	if r.DockerClient != nil {
		return r.DockerClient, nil
	}
	return dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
}

// LogsHandler returns an http.Handler that can be registered on the manager.
func (r *DockerContainerReconciler) LogsHandler() http.Handler {
	return http.HandlerFunc(r.HandleLogs)
}

// GetContainerLogs is a programmatic API to fetch container logs (non-streaming).
func (r *DockerContainerReconciler) GetContainerLogs(ctx context.Context, containerName string, tail string) (string, error) {
	cli, err := r.getLogsClient()
	if err != nil {
		return "", err
	}

	if tail == "" {
		tail = "100"
	}

	reader, err := cli.ContainerLogs(ctx, containerName, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
	})
	if err != nil {
		return "", err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
