FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /okdp-control-plane-server ./cmd/server

FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
    && addgroup -S okdp && adduser -S -G okdp okdp

COPY --from=builder /okdp-control-plane-server /usr/local/bin/okdp-control-plane-server

USER okdp

EXPOSE 8093

ENTRYPOINT ["/usr/local/bin/okdp-control-plane-server"]
