# LocalRouter container image: multi-stage, static binary, minimal runtime.
#
# Stage 1 builds a fully static (CGO_ENABLED=0) localrouter binary with the same
# bounded build parallelism the native release script already uses
# (GOMAXPROCS=2, GOFLAGS=-p=2, see scripts/release.sh), so a container build
# never fans out across every host core.
#
# Stage 2 is a digest-pinned minimal Alpine runtime carrying a CA bundle (the router
# proxies to upstream providers over HTTPS), an explicit non-root UID/GID
# 1000:1000, and busybox wget as the health-probe helper (no curl is installed).
# This image never terminates TLS: no tls_cert_file/tls_key_file are configured
# for it, and it publishes no host port. Terminate TLS in front of it (NGINX)
# and publish only there.
#
# Expected layout. Every path below is a bind mount supplied by the operator;
# nothing is declared VOLUME, so a read-only root filesystem stays usable.
#   /etc/localrouter/config.yaml   ro   config (the default -config path)
#   /etc/localrouter/pricing.yaml  ro   imported price table
#   /run/secrets/*                 ro   client bearer keys, provider API keys
#   /var/lib/localrouter           rw   THE ONLY WRITABLE PATH: SQLite ledger,
#                                       rotated OAuth tokens, collector state
#   /tmp                           tmpfs (run with --tmpfs /tmp)
#
# Example (no host port published; publish from the TLS terminator instead):
#   podman build --jobs 2 --memory 6g -t localhost/localrouter:0.3.0 .
#   podman run --read-only --tmpfs /tmp \
#     -v ./config.yaml:/etc/localrouter/config.yaml:ro \
#     -v ./pricing.yaml:/etc/localrouter/pricing.yaml:ro \
#     -v ./secrets:/run/secrets:ro \
#     -v localrouter-data:/var/lib/localrouter:rw \
#     localhost/localrouter:0.3.0

# Base images are pinned to the multi-arch index digest; the tag is kept for
# readability and is ignored when a digest is present. Dependabot (docker
# ecosystem) proposes digest bumps. To update by hand:
#   skopeo inspect --raw docker://docker.io/library/golang:<tag> | sha256sum
FROM docker.io/library/golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build

# Static binary; bounded parallelism matches the release contract.
ENV CGO_ENABLED=0 \
    GOFLAGS=-p=2 \
    GOMAXPROCS=2

WORKDIR /src

# Module layer first so dependency downloads are cached independently of the
# source tree. go.mod/go.sum are small; the build context is trimmed by
# .containerignore (no .git, keys, tokens, data files or docs).
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION stamps only the OCI label below. The binary has no wired version
# symbol (scripts/release.sh injects no linker flags for the same reason), so
# nothing is fabricated into the executable.
ARG VERSION=dev

RUN go build -trimpath -o /out/localrouter ./cmd/localrouter \
 && /out/localrouter --help > /dev/null

FROM docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS runtime

ARG VERSION=dev

LABEL org.opencontainers.image.title="localrouter" \
      org.opencontainers.image.description="Quota-aware local LLM gateway; HTTP only, terminate TLS in front" \
      org.opencontainers.image.source="https://github.com/hpst3r/localrouter" \
      org.opencontainers.image.version="${VERSION}"

# ca-certificates is required: the router proxies to upstream providers over
# HTTPS. The image's own listener is plain HTTP. A fixed non-root UID/GID keeps
# the process identity explicit rather than inheriting podman's userns root.
# ca-certificates is not version-pinned (Alpine drops superseded package
# versions from its mirrors); the base digest fixes the release branch, so
# the apk layer is not bit-for-bit reproducible across rebuilds.
RUN apk add --no-cache ca-certificates \
 && addgroup -g 1000 -S localrouter \
 && adduser -u 1000 -S -D -H -G localrouter -s /sbin/nologin localrouter \
 && mkdir -p /etc/localrouter /run/secrets /var/lib/localrouter \
 && chown 1000:1000 /var/lib/localrouter

COPY --from=build /out/localrouter /usr/local/bin/localrouter

# HOME must be set (an unset HOME makes os.UserConfigDir fall back to the cwd,
# which is read-only here) and points at the single writable root so any
# $HOME-relative default resolves somewhere writable. The shipped config still
# pins data_dir explicitly.
ENV HOME=/var/lib/localrouter
WORKDIR /var/lib/localrouter

USER 1000:1000

# Health probe helper: busybox wget ships with Alpine. /readyz is
# unauthenticated and reports local storage health, so it is the right probe
# (never a liveness-only /healthz claim). It exits non-zero on any non-2xx.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8787/readyz || exit 1

# No EXPOSE and no published port: the terminator in front owns the host port.
ENTRYPOINT ["/usr/local/bin/localrouter"]
CMD ["serve", "-config", "/etc/localrouter/config.yaml"]
