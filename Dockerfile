# syntax=docker/dockerfile:1.6

FROM --platform=$BUILDPLATFORM golang:1.26.5-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/codeq-server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/osvaldoandrade/codeq" \
      org.opencontainers.image.title="codeq-service" \
      org.opencontainers.image.description="codeQ task queue server"

WORKDIR /app
COPY --from=build /out/codeq-server /app/codeq-server

# Pre-create writable data dirs owned by nonroot. Distroless has no shell,
# so we materialize the directory tree by COPYing a placeholder; docker
# preserves the parent-dir ownership for any volume mounted on top.
COPY --from=build --chown=nonroot:nonroot /out/codeq-server /var/lib/codeq/pebble/.keep
COPY --from=build --chown=nonroot:nonroot /out/codeq-server /var/lib/codeq/artifacts/.keep

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/codeq-server"]
