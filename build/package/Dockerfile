FROM golang:1.25.7 AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src

RUN export GOOS="${TARGETOS:-$(go env GOOS)}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    && CGO_ENABLED=0 go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/shiftpv-controller ./src/cmd/controller
RUN export GOOS="${TARGETOS:-$(go env GOOS)}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
    && CGO_ENABLED=0 go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/shiftpv-node ./src/cmd/node

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates mount \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/shiftpv-controller /shiftpv-controller
COPY --from=build /out/shiftpv-node /shiftpv-node
USER 65532:65532
ENTRYPOINT ["/shiftpv-controller"]
