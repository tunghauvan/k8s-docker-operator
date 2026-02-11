package controller

import (
	"context"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// MockDockerClient implements dockerclient.APIClient for testing
type MockDockerClient struct {
	client.APIClient
	Containers []types.Container
	Created    []string
	Removed    []string
	Started    []string
}

func (m *MockDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error) {
	return m.Containers, nil
}

func (m *MockDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *specs.Platform, containerName string) (container.CreateResponse, error) {
	m.Created = append(m.Created, containerName)
	id := "mock-id-" + containerName
	// Add to containers list
	m.Containers = append(m.Containers, types.Container{
		ID:    id,
		Names: []string{"/" + containerName},
		Image: config.Image,
		State: "created",
	})
	return container.CreateResponse{ID: id}, nil
}

func (m *MockDockerClient) ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error {
	m.Started = append(m.Started, containerID)
	// Update state
	for i := range m.Containers {
		if m.Containers[i].ID == containerID {
			m.Containers[i].State = "running"
		}
	}
	return nil
}

func (m *MockDockerClient) ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error {
	return nil
}

func (m *MockDockerClient) ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error {
	m.Removed = append(m.Removed, containerID)
	// Remove from list if present (simple mock)
	for i, c := range m.Containers {
		if c.ID == containerID {
			m.Containers = append(m.Containers[:i], m.Containers[i+1:]...)
			break
		}
	}
	// Also handle name removal mock
	return nil
}

func (m *MockDockerClient) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
