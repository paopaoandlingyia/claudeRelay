# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.23

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/claude-relay ./cmd/claude-relay

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 claude-relay \
    && adduser -S -D -H -u 10001 -G claude-relay claude-relay

WORKDIR /app
COPY --from=build --chown=10001:10001 /out/claude-relay /app/claude-relay
COPY --chown=10001:10001 deploy/config.container.json /app/config.json
RUN mkdir -p /data && chown 10001:10001 /data

USER 10001:10001
EXPOSE 8567

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8567/healthz || exit 1

ENTRYPOINT ["/app/claude-relay"]
CMD ["serve", "-config", "/app/config.json"]
