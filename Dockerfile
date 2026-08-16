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
ARG TEPLOY_VERSION=v0.1.26
RUN wget -qO- "https://github.com/useteploy/teploy-cli/releases/download/${TEPLOY_VERSION}/teploy_linux_amd64.tar.gz" \
    | tar xz -C /usr/local/bin teploy \
    && chmod +x /usr/local/bin/teploy

COPY --from=builder /teploy-dash /usr/local/bin/teploy-dash

EXPOSE 3456
VOLUME ["/var/teploy-dash"]

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:3456/api/health || exit 1

ENTRYPOINT ["teploy-dash"]
