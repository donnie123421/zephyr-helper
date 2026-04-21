# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/donnie123421/zephyr-helper/internal/version.Version=${VERSION}" \
    -o /out/zephyr-helper ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 zephyr
USER zephyr
COPY --from=builder /out/zephyr-helper /usr/local/bin/zephyr-helper
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/zephyr-helper"]
