# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22

# ---------- build ----------
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src
ENV CGO_ENABLED=0

# Cache the module layer separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/quickproxy ./cmd/quickproxy

# ---------- runtime ----------
FROM alpine:${ALPINE_VERSION}

LABEL org.opencontainers.image.title="aws-quickwork-2api" \
      org.opencontainers.image.description="Amazon Quick / QuickWork to OpenAI-compatible API with a password-protected /admin console" \
      org.opencontainers.image.source="https://github.com/qingdeng888/aws-quickwork-2api" \
      org.opencontainers.image.licenses="GPL-3.0-only"

# ca-certificates: TLS to the upstream and OIDC endpoints.
# tzdata: config.json "timezone" is validated against the IANA database.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /app qw \
 && mkdir -p /app/data \
 && chown -R qw:qw /app

WORKDIR /app

COPY --from=build /out/quickproxy /app/quickproxy
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh \
 && sed -i 's/\r$//' /usr/local/bin/entrypoint.sh \
 && test "$(head -c 2 /usr/local/bin/entrypoint.sh)" = '#!'

USER qw

# The container must listen on a routable address; 127.0.0.1 would make the
# published port unreachable. Override ADDR only if you also change the port
# mapping and the health check.
ENV CONFIG_PATH=/app/data/config.json \
    DATA_DIR=/app/data \
    DATABASE=/app/data/quickwork.db \
    ADDR=0.0.0.0:8080 \
    QW_CONTAINER=1 \
    TZ=UTC

VOLUME ["/app/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

# Run the entrypoint through /bin/sh explicitly: it keeps working even if a
# checkout loses the executable bit or the shebang line endings drift.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/entrypoint.sh"]
CMD ["server"]
