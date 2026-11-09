# syntax=docker/dockerfile:1
#
# Open Fabrica container image. The binary is pure Go (no cgo), so the build is a
# plain static compile; the runtime layer adds chromium for the post-build runtime
# validator and PDF generation.
#
# Recommended run (the --memory cap + auto-restart are the hard backstop for a
# runaway model-authored server function; goja has no per-VM heap cap of its own):
#
#   docker build -t openfabrica .
#   docker run --rm \
#     --memory=2g --memory-swap=2g --restart=on-failure \
#     -e GOMEMLIMIT=1750MiB \
#     -v openfabrica-data:/data \
#     -p 5733:5733 \
#     openfabrica
#
# The admin panel (5734) binds to loopback by default, so it is only reachable
# from inside the container. To reach it, either `docker exec` in and tunnel, or
# expose it deliberately with `-e OPENFABRICA_ADMIN_BIND=0.0.0.0 -e
# OPENFABRICA_ADMIN_EXPOSE=1 -p 5734:5734` behind your own auth/firewall.
# For public HTTPS, also publish `-p 80:80 -p 443:443` so on-demand Let's Encrypt
# can bind them.

# --- Build stage: compile the single static binary ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/openfabrica ./cmd/openfabrica

# --- Runtime stage ---
FROM alpine:3.20
# chromium powers the runtime validator + PDF generation; the font package gives
# headless Chrome something to render. ca-certificates is needed for outbound
# HTTPS (LLM providers and Let's Encrypt).
RUN apk add --no-cache chromium ca-certificates ttf-freefont libcap

# Run as an unprivileged user. The capability lets it still bind :80/:443 for
# on-demand HTTPS without being root.
RUN adduser -D -u 10001 fabrica \
 && mkdir -p /data \
 && chown fabrica:fabrica /data

WORKDIR /app
COPY --from=build /out/openfabrica /app/openfabrica
RUN setcap cap_net_bind_service=+ep /app/openfabrica \
 && chown fabrica:fabrica /app
# /app is owned by the runtime user so the self-destruct command can remove the
# binary; a restart policy then cannot bring up a fresh empty instance.

# Projects, keys, and the database live here; mount a volume so they survive
# container restarts. Back up this directory together with its encryption key.
ENV OPENFABRICA_DATA_DIR=/data
VOLUME ["/data"]

# Soft heap ceiling: keeps GC pressuring toward the limit before the container's
# hard --memory cap OOM-kills the process. Set to ~80-90% of the --memory value.
ENV GOMEMLIMIT=1750MiB

# Don't try to open a browser tab on a headless host.
ENV OPENFABRICA_NO_BROWSER=1

# 5733 = public project server, 5734 = admin panel (loopback-only by default).
EXPOSE 5733 5734

# Liveness: the admin listener answers /healthz without touching the database. It is
# probed on loopback, which is always reachable from inside the container.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${OPENFABRICA_ADMIN_PORT:-5734}/healthz" || exit 1

USER fabrica
ENTRYPOINT ["/app/openfabrica"]
