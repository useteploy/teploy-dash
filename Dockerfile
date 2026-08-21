FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /teploy-dash \
        ./cmd/teploy-dash

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget

# Teploy CLI — used by dash to delegate deploy/rollback actions.
#
# Verified against checksums.txt before extraction, and resolved through
# TARGETARCH, matching Dockerfile.goreleaser. This file previously piped the
# download straight into tar on a hardcoded amd64 URL — and this is the file a
# source build uses, so the image running in production was the one NOT
# checking what it executed.
ARG TARGETARCH
ARG TEPLOY_VERSION=v0.1.27
RUN set -eux; \
    archive="teploy_linux_${TARGETARCH:-amd64}.tar.gz"; \
    release_url="https://github.com/useteploy/teploy-cli/releases/download/${TEPLOY_VERSION}"; \
    wget -qO "/tmp/${archive}" "${release_url}/${archive}"; \
    wget -qO /tmp/checksums.txt "${release_url}/checksums.txt"; \
    grep "  ${archive}$" /tmp/checksums.txt > /tmp/teploy.sha256; \
    (cd /tmp && sha256sum -c teploy.sha256); \
    tar xzf "/tmp/${archive}" -C /usr/local/bin teploy; \
    chmod +x /usr/local/bin/teploy; \
    rm -f "/tmp/${archive}" /tmp/checksums.txt /tmp/teploy.sha256; \
    teploy version

COPY --from=builder /teploy-dash /usr/local/bin/teploy-dash

EXPOSE 3456
VOLUME ["/var/teploy-dash"]

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:3456/api/health || exit 1

ENTRYPOINT ["teploy-dash"]
