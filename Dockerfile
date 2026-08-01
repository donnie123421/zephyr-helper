# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/donnie123421/zephyr-helper/internal/version.Version=${VERSION} -X github.com/donnie123421/zephyr-helper/internal/version.Commit=${COMMIT}" \
    -o /out/zephyr-helper ./cmd/server

FROM alpine:3.24
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 zephyr \
    # Pre-create the tsnet state dir owned by zephyr so the named
    # docker volume mounted at /tmp/tsnet inherits this ownership
    # on first mount. Without it, the volume comes up owned by
    # root and tsnet can't write its node key — registration silently
    # fails and the helper never joins the tailnet.
    && mkdir -p /tmp/tsnet \
    && chown -R zephyr:zephyr /tmp/tsnet \
    # Same trick for /data (events DB + push prefs): the iOS installer
    # mounts a named volume here, and pre-creating the dir hands the
    # volume zephyr ownership on first mount. Deploys without the mount
    # still get a writable-but-ephemeral /data inside the container FS.
    && mkdir -p /data \
    && chown -R zephyr:zephyr /data
USER zephyr
COPY --from=builder /out/zephyr-helper /usr/local/bin/zephyr-helper
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/zephyr-helper"]
