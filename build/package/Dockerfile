FROM golang:1.25.7 AS build-base

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src

FROM build-base AS build-controller
RUN export GOOS="${TARGETOS:-$(go env GOOS)}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    && CGO_ENABLED=0 go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/shiftpv-controller ./src/cmd/controller

FROM build-base AS build-node
RUN export GOOS="${TARGETOS:-$(go env GOOS)}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    && CGO_ENABLED=0 go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/shiftpv-node ./src/cmd/node

FROM debian:bookworm-slim AS runtime-base
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

FROM runtime-base AS controller
COPY --from=build-controller /out/shiftpv-controller /shiftpv-controller
USER 65532:65532
ENTRYPOINT ["/shiftpv-controller"]

FROM runtime-base AS node
RUN apt-get update \
    && apt-get install -y --no-install-recommends mount \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build-node /out/shiftpv-node /shiftpv-node
USER 0:0
ENTRYPOINT ["/shiftpv-node"]

# Keep the current single-image Helm and kind flow working until the chart is
# switched to the independent controller and node image references.
FROM runtime-base AS combined
RUN apt-get update \
    && apt-get install -y --no-install-recommends mount \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build-controller /out/shiftpv-controller /shiftpv-controller
COPY --from=build-node /out/shiftpv-node /shiftpv-node
USER 65532:65532
ENTRYPOINT ["/shiftpv-controller"]
