.PHONY: dev clean build build-images run-proxy run-target test unit-test docker-build-proxy docker-build-target docker-build-init docker-build-python deploy load-images undeploy kind-create kind-delete

KIND_CLUSTER_NAME ?= kagenti # default to kagenti cluster name

# Docker build targets
docker-build-proxy:
	podman build -t auth-proxy:latest .

docker-build-target:
	cd quickstart/demo-app && podman build -t demo-app:latest -f Dockerfile .

docker-build-init:
	podman build -f Dockerfile.init -t proxy-init:latest .

# Build demo Docker images (auth-proxy, demo-app, proxy-init).
# The authbridge-unified sidecar image is built from cmd/authbridge/Dockerfile:
#   cd authbridge && podman build -f cmd/authbridge/Dockerfile -t authbridge-unified:latest .
build-images: docker-build-proxy docker-build-target docker-build-init

# Load Docker images into kind cluster and re-tag for podman compatibility.
# Podman tags images with a localhost/ prefix, but Kubernetes resolves bare
# image names to docker.io/library/. Re-tagging inside the node ensures the
# images are found when pods are scheduled.
KIND_NODE ?= $(KIND_CLUSTER_NAME)-control-plane
LOCAL_IMAGES = auth-proxy demo-app proxy-init
load-images:
	@for img in $(LOCAL_IMAGES); do \
		kind load docker-image $$img:latest --name $(KIND_CLUSTER_NAME); \
		podman exec $(KIND_NODE) ctr --namespace=k8s.io images tag localhost/$$img:latest docker.io/library/$$img:latest 2>/dev/null || true; \
	done

# Deploy to Kubernetes
deploy:
	kubectl apply -f quickstart/k8s/demo-app-deployment.yaml
	kubectl apply -f k8s/auth-proxy-deployment.yaml

# Remove deployment from Kubernetes
undeploy:
	kubectl delete -f k8s/auth-proxy-deployment.yaml --ignore-not-found=true
	kubectl delete -f quickstart/k8s/demo-app-deployment.yaml --ignore-not-found=true

# Kind cluster management
kind-create:
	kind create cluster --name $(KIND_CLUSTER_NAME)

kind-delete:
	kind delete cluster --name $(KIND_CLUSTER_NAME)

# Run Go unit tests
unit-test:
	go test ./... -v

# Integration test via curl
test:
	@echo "Testing valid authorization..."
	@curl -s -H "Authorization: kagenti" http://localhost:8080/test
	@echo ""
	@echo "Testing invalid authorization..."
	@curl -s -H "Authorization: invalid" http://localhost:8080/test
	@echo ""
	@echo "Testing no authorization..."
	@curl -s http://localhost:8080/test
	@echo ""
