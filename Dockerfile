ARG GO_VERSION=1.27

# Cross-compile on the native build platform (no QEMU emulation): the Go
# toolchain runs natively and emits a static binary for the target arch.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS go-build

ARG TARGETOS=linux
ARG TARGETARCH
# Version of the server (no leading "v"), named in the startup log and in the
# co-author trailer of the commits it writes. CI passes the release version.
ARG VERSION=dev

WORKDIR /workspace/okdp-server

# go.mod replaces github.com/okdp/okdp-compiler with ../okdp-compiler until it
# is published: build with --build-context okdp-compiler=../okdp-compiler. The
# module root is the package (compiler.go), its implementation internal/.
COPY --from=okdp-compiler go.mod go.sum *.go ../okdp-compiler/
COPY --from=okdp-compiler internal/ ../okdp-compiler/internal/
# The same for github.com/okdp/okdp-gitops: --build-context
# okdp-gitops=../okdp-gitops. Its packages, without its command; its own
# go.mod replaces okdp-compiler with ../okdp-compiler, the directory above.
COPY --from=okdp-gitops go.mod go.sum ../okdp-gitops/
COPY --from=okdp-gitops catalog/ ../okdp-gitops/catalog/
COPY --from=okdp-gitops charts/ ../okdp-gitops/charts/
COPY --from=okdp-gitops compile/ ../okdp-gitops/compile/
COPY --from=okdp-gitops gitops/ ../okdp-gitops/gitops/
COPY --from=okdp-gitops internal/ ../okdp-gitops/internal/
COPY go.* ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/okdp/okdp-control-plane-server/internal/buildinfo.Version=${VERSION}" \
      -o /okdp-server ./cmd/server

FROM alpine:3.21

RUN apk --no-cache add ca-certificates && update-ca-certificates

COPY --from=go-build /okdp-server /usr/local/bin/okdp-server

# Writable HOME for the unprivileged user (Git and registry clients).
ENV HOME=/tmp

USER 65534:65534

EXPOSE 8093

ENTRYPOINT ["okdp-server"]
