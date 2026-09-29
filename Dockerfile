# Multi-stage build: static compilation (CGO disabled, SQLite uses a pure-Go driver) -> minimal runtime image.
# Build:  docker build -t aigate:0.1.0 .
# Run:    docker run -p 8080:8080 -e AIGATE_INIT_ADMIN_TOKEN=<TOKEN> aigate:0.1.0
#         Defaults to in-container SQLite (/data/aigate.db); for production override with
#         `docker run ... aigate:0.1.0 -db-type mysql -db-dsn "<DSN>"`.

FROM golang:1.24 AS build

WORKDIR /src

# Fetch dependencies first to maximize layer cache hits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /aigate ./cmd/aigate

FROM alpine:3.20

# Root certificates for HTTPS upstreams; tzdata for business timezones.
RUN apk add --no-cache ca-certificates tzdata

# Run as non-root (UID/GID 65532, matching the distroless nonroot convention) and prepare a writable data directory.
RUN mkdir -p /data && chown 65532:65532 /data

COPY --from=build /aigate /aigate

USER 65532:65532

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

# Default arguments (SQLite lives in /data); append arguments to override, e.g.
#   docker run aigate:0.1.0 -db-type mysql -db-dsn "<DSN>"
CMD ["-db-dsn", "/data/aigate.db"]

ENTRYPOINT ["/aigate"]
