# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/astrolabe ./cmd/astrolabe

FROM alpine:3.22
# The container usually runs as your own uid (compose sets user: from .env)
# so files it writes in bind mounts belong to you. /data is writable by any
# uid; secret files themselves are created 0600.
RUN apk add --no-cache ca-certificates wget \
 && adduser -D -H -u 10001 astrolabe \
 && mkdir -p /data/products /data/secrets \
 && chmod 1777 /data /data/products /data/secrets
COPY --from=build /out/astrolabe /usr/local/bin/astrolabe
USER astrolabe
WORKDIR /data
ENV ASTROLABE_ADDR=0.0.0.0:8080 \
    ASTROLABE_PRODUCTS=/data/products \
    ASTROLABE_SECRETS_DIR=/data/secrets
EXPOSE 8080
VOLUME ["/data/products", "/data/secrets"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["astrolabe"]
CMD ["serve"]
