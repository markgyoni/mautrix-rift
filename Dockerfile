# The builder always runs on the build machine's platform and cross-compiles (cgo included, for
# SQLite) with tonistiigi/xx, so arm64 images don't need slow emulation.
FROM --platform=$BUILDPLATFORM tonistiigi/xx:1.6.1 AS xx

FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24 AS builder
COPY --from=xx / /
RUN apk add --no-cache git clang lld
ARG TARGETPLATFORM
RUN xx-apk add --no-cache musl-dev gcc
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# .git isn't in the build context, so CI passes the version info in.
ARG TAG=unknown
ARG COMMIT=unknown
RUN xx-go --wrap && TAG="$TAG" COMMIT="$COMMIT" ./build.sh && xx-verify mautrix-rift

FROM alpine:3.24.4
RUN apk add --no-cache ca-certificates tzdata \
	&& adduser -D -H -u 1337 mautrix
COPY --from=builder /build/mautrix-rift /usr/bin/mautrix-rift
COPY --chmod=755 docker/run.sh /usr/bin/run-bridge
USER 1337:1337
VOLUME /data
WORKDIR /data
# Unhealthy while waiting for a new bbctl config (see docker/run.sh).
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD test ! -e /tmp/needs-bbctl-config
# bbctl configs embed the registration and connect over a websocket, so no port or -r is needed.
CMD ["/usr/bin/run-bridge"]
