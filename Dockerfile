ARG GO_VERSION=1.25.0

FROM golang:${GO_VERSION} AS go-build

ARG GIT_COMMIT="_unset_"
ARG LDFLAGS="-X localbuild=true"
ARG TARGETOS="linux"
ARG TARGETARCH

WORKDIR /workspace/okdp-control-plane-server

# Copy dependency files first (better layer caching)
COPY go.mod go.sum ./
RUN go mod download

COPY tools.go ./
COPY cmd/ cmd/
COPY docs/ docs/
COPY internal/ internal/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    LDFLAGS=${LDFLAGS##-X localbuild=true} GIT_COMMIT=$GIT_COMMIT \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -a -o okdp-control-plane-server cmd/server/main.go
    
FROM alpine:3.21.3

RUN apk --no-cache add ca-certificates && update-ca-certificates

COPY --from=go-build --chown=65534:65534 /workspace/okdp-control-plane-server/okdp-control-plane-server /usr/local/bin/okdp-control-plane-server

USER 65534:65534

EXPOSE 8093

ENTRYPOINT ["okdp-control-plane-server"]
