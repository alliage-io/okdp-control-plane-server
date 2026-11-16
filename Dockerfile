# Base images by tag and digest (the multi-arch index): the digest is what
# is pulled, the tag says which it is; Dependabot (docker) bumps both. The
# Go toolchain is go.mod's go version.

# Cross-compile on the native build platform (no QEMU emulation): the Go
# toolchain runs natively and emits a static binary for the target arch.
FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS go-build

ARG TARGETOS=linux
ARG TARGETARCH
# Version of the server (no leading "v"), named in the startup log and in the
# co-author trailer of the commits it writes. CI passes the release version.
ARG VERSION=dev

WORKDIR /workspace/okdp-server

# go.mod replaces github.com/okdp/okdp-compiler and github.com/okdp/okdp-gitops
# with ../okdp-compiler and ../okdp-gitops until they are published: build with
# --build-context okdp-compiler=../okdp-compiler --build-context
# okdp-gitops=../okdp-gitops. Each module is copied whole (okdp-gitops' own
# go.mod replaces okdp-compiler with ../okdp-compiler, the directory above), so
# a refactor of a sibling does not break this list; only what the build never
# reads is left out: their Git metadata, binaries and test data.
COPY --from=okdp-compiler --exclude=.git --exclude=bin --exclude=**/testdata . ../okdp-compiler/
COPY --from=okdp-gitops --exclude=.git --exclude=bin --exclude=**/testdata . ../okdp-gitops/
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

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk --no-cache add ca-certificates && update-ca-certificates

COPY --from=go-build /okdp-server /usr/local/bin/okdp-server

# Writable HOME for the unprivileged user (Git and registry clients).
ENV HOME=/tmp

USER 65534:65534

EXPOSE 8093

ENTRYPOINT ["okdp-server"]
