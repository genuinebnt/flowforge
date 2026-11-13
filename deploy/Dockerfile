# syntax=docker/dockerfile:1.7
#
# One image per Flowforge binary (spec 17.1, 18.2):
#
#   docker build -f deploy/Dockerfile --build-arg SERVICE=engine -t ff-engine .
#
# SERVICE selects ./cmd/<SERVICE> (gateway, engine, scope, graph, notifier,
# similarity, content, tool-runner, egress-proxy, migrate). The final image is
# distroless static, runs as nonroot (65532), holds a single static binary and
# writes nothing to its filesystem, so it runs with a read-only root
# (docker run --read-only, Kubernetes readOnlyRootFilesystem).

ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

ENV CGO_ENABLED=0 \
    GOFLAGS=-trimpath \
    GOTOOLCHAIN=local

# Module download is cached separately from the source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

COPY . .

ARG SERVICE
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

RUN if [ -z "${SERVICE}" ]; then echo "build arg SERVICE is required" >&2; exit 1; fi; \
    if [ ! -d "./cmd/${SERVICE}" ]; then echo "./cmd/${SERVICE} does not exist" >&2; exit 1; fi

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/app "./cmd/${SERVICE}"

FROM gcr.io/distroless/static-debian12:nonroot

ARG SERVICE
LABEL org.opencontainers.image.title="flowforge-${SERVICE}" \
      org.opencontainers.image.source="https://github.com/genuinebnt/flowforge" \
      org.opencontainers.image.licenses="UNLICENSED"

COPY --from=build --chown=0:0 --chmod=0555 /out/app /app

USER 65532:65532
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app"]
