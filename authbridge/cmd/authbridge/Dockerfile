# AuthBridge unified image — Envoy + authbridge binary in one container.
# Drop-in replacement for envoy-with-processor (Dockerfile.envoy).
#
# Build context: ./authbridge (needs access to both authlib/ and cmd/authbridge/)

# Stage 1: Build authbridge binary
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# Copy both modules with Docker-compatible layout
COPY authlib/ authlib/
COPY cmd/authbridge/ cmd/authbridge/

# The go.mod has: replace authlib => ../../authlib
# In Docker the layout is /app/authlib and /app/cmd/authbridge, so the
# relative path ../../authlib resolves to /app/authlib. Correct.
ENV GOWORK=off
RUN cd cmd/authbridge && CGO_ENABLED=0 GOOS=linux go build -o /authbridge .

# Stage 2: Get Envoy binary
FROM docker.io/envoyproxy/envoy:v1.37.1 AS envoy-source

# Stage 3: Runtime — same base as Dockerfile.envoy
FROM registry.access.redhat.com/ubi9/ubi-minimal@sha256:83006d535923fcf1345067873524a3980316f51794f01d8655be55d6e9387183

RUN microdnf install -y ca-certificates && microdnf clean all

COPY --from=envoy-source /usr/local/bin/envoy /usr/local/bin/envoy
COPY --from=builder /authbridge /usr/local/bin/authbridge
COPY cmd/authbridge/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod 755 /usr/local/bin/entrypoint.sh \
              /usr/local/bin/envoy \
              /usr/local/bin/authbridge

# Envoy writes hot-restart lock files and admin socket here
RUN mkdir -p /tmp/envoy && chmod 775 /tmp/envoy

USER 1337

EXPOSE 15123 15124 9901 9090

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
