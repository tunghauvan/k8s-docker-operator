# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

all: build

# Run tests
test:
	go test ./... -coverprofile cover.out

# Build manager binary
build:
	go build -o bin/manager cmd/manager/main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
run: install
	go run ./cmd/manager/main.go

# Install CRDs into a cluster
install:
	kubectl apply -f config/crd/bases/app.example.com_composeapps.yaml

# Uninstall CRDs from a cluster
uninstall:
	kubectl delete -f config/crd/bases/app.example.com_composeapps.yaml

# Build the docker image
docker-build:
	docker build . -t ${IMG}

# Push the docker image
docker-push:
	docker push ${IMG}
