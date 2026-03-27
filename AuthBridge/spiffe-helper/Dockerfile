ARG SPIFFE_HELPER_VERSION=v0.11.0

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS source
ARG SPIFFE_HELPER_VERSION
WORKDIR /workspace
ARG SPIFFE_HELPER_SHA=1d0551d63787b528926b3e17fac949a376040bec
RUN apk add --no-cache git && \
    git clone --depth 1 --branch ${SPIFFE_HELPER_VERSION} \
        https://github.com/spiffe/spiffe-helper.git . && \
    test "$(git rev-parse HEAD)" = "${SPIFFE_HELPER_SHA}"

FROM --platform=$BUILDPLATFORM source AS builder
ARG TARGETARCH
ENV CGO_ENABLED=0 GOARCH=${TARGETARCH}
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -o bin/spiffe-helper cmd/spiffe-helper/main.go && \
    chmod 755 bin/spiffe-helper

FROM registry.access.redhat.com/ubi9/ubi-micro@sha256:2173487b3b72b1a7b11edc908e9bbf1726f9df46a4f78fd6d19a2bab0a701f38

COPY --from=builder /workspace/bin/spiffe-helper /spiffe-helper

USER 1001

ENTRYPOINT ["/spiffe-helper"]
CMD []
