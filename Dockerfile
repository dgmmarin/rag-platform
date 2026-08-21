# syntax=docker/dockerfile:1

# Build stage: compile a static ragctl binary.
FROM golang:1.22-bookworm AS build
WORKDIR /src

# Cache modules first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Build.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-trimpath \
    go build -ldflags="-s -w" -o /out/ragctl ./cmd/ragctl

# Runtime stage: distroless static, non-root (ADR-0002 small static images).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ragctl /usr/local/bin/ragctl
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ragctl"]
# Default to the API server; override with `work`, `migrate`, etc.
CMD ["serve"]
