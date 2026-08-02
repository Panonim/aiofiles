# aiofiles - self-hosted media downloader / converter / compressor.
#
# One container, two supervised processes (nginx + the Go API) under
# s6-overlay. Everything downloaded at build time is pinned to an explicit
# version and verified against a SHA256 checksum; bump the ARGs below to
# upgrade. Nothing is fetched at run time.
#
#   docker build -t aiofiles .
#   docker buildx build --platform linux/amd64,linux/arm64 -t aiofiles .

# --- pinned versions -------------------------------------------------------
ARG ALPINE_VERSION=3.24
ARG GO_VERSION=1.25

# Alpine.js 3.x, minified browser build (npm/jsDelivr).
ARG ALPINEJS_VERSION=3.15.12
ARG ALPINEJS_SHA256=57b37d7cae9a27d965fdae4adcc844245dfdc407e655aee85dcfff3a08036a3f

# Lucide UMD build - defines window.lucide.
ARG LUCIDE_VERSION=1.28.0
ARG LUCIDE_SHA256=9723deed8adf7dbc3a2579927d7b724e44001ba12280025d66defc57d1cd8316

# yt-dlp standalone binary. NOTE: the asset is `yt-dlp_musllinux`, NOT
# `yt-dlp_linux` - the latter is the glibc/manylinux PyInstaller build and will
# not start on Alpine, and the plain `yt-dlp` asset is a zipapp that needs a
# Python interpreter. Checksums come from the release's SHA2-256SUMS file.
ARG YTDLP_VERSION=2026.07.04
ARG YTDLP_SHA256_AMD64=f7439ec2e3ffe69e06ac233f83f0d9687b89105939129bddcbf74e5de0f2b40e
ARG YTDLP_SHA256_ARM64=9a6a4de88f35dc68c1763945fbb417e092ebd9afc5d66052ac31b68d405a12a7

# s6-overlay v3. Checksums come from the published .sha256 release assets.
ARG S6_OVERLAY_VERSION=3.2.3.2
ARG S6_SHA256_NOARCH=5379750ed30a84bbd2e2dd74847ba6b5bd29cd0b2e3ea2ec58049b57eb2eda12
ARG S6_SHA256_X86_64=e6befcc96a437a3831386ecfc51808c5d3e939dc5fe3c02ae9284599e8aa2408
ARG S6_SHA256_AARCH64=b17f17a82e7a515c682a91edaf2ffdabb73f891981b6c1fd712115693a2f8b4c


# ===========================================================================
# Stage 1 - build the static Go binary
#
# Runs on the BUILD platform and cross-compiles via GOARCH, so an arm64 image
# does not have to be produced under qemu. CGO_ENABLED=0 is what keeps the
# binary static (and is why modernc.org/sqlite is used instead of mattn).
# ===========================================================================
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:${GO_VERSION}-alpine AS gobuild

WORKDIR /src

# Dependency layer first so it survives every source-only change.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETARCH
RUN set -eu; \
    if [ -n "${TARGETARCH:-}" ]; then export GOARCH="${TARGETARCH}"; fi; \
    export CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath; \
    go build -ldflags="-s -w" -o /out/aiofiles ./cmd/server; \
    test -s /out/aiofiles


# ===========================================================================
# Stage 2 - fetch and verify everything that comes from the network
#
# Also unpacks s6-overlay here rather than in the final stage: it keeps curl,
# tar and xz out of the runtime image entirely, and it means the whole
# download set is verified in one place. Runs on the BUILD platform; the
# target architecture only selects which asset is fetched.
# ===========================================================================
FROM --platform=${BUILDPLATFORM:-linux/amd64} alpine:${ALPINE_VERSION} AS assets

RUN apk add --no-cache curl ca-certificates tar xz

ARG TARGETARCH
ARG ALPINEJS_VERSION
ARG ALPINEJS_SHA256
ARG LUCIDE_VERSION
ARG LUCIDE_SHA256
ARG YTDLP_VERSION
ARG YTDLP_SHA256_AMD64
ARG YTDLP_SHA256_ARM64
ARG S6_OVERLAY_VERSION
ARG S6_SHA256_NOARCH
ARG S6_SHA256_X86_64
ARG S6_SHA256_AARCH64

# --- frontend vendor assets ---
# curl -f already rejects 4xx/5xx; the size floors catch a CDN that answers
# 200 with an error page, and the checksums catch everything else.
RUN set -eu; \
    mkdir -p /vendor; \
    curl -fsSL --retry 3 --retry-delay 2 -o /vendor/alpine.min.js \
        "https://cdn.jsdelivr.net/npm/alpinejs@${ALPINEJS_VERSION}/dist/cdn.min.js"; \
    curl -fsSL --retry 3 --retry-delay 2 -o /vendor/lucide.min.js \
        "https://cdn.jsdelivr.net/npm/lucide@${LUCIDE_VERSION}/dist/umd/lucide.min.js"; \
    test "$(stat -c%s /vendor/alpine.min.js)" -gt 10000; \
    test "$(stat -c%s /vendor/lucide.min.js)" -gt 50000; \
    printf '%s  %s\n' "${ALPINEJS_SHA256}" /vendor/alpine.min.js | sha256sum -c -; \
    printf '%s  %s\n' "${LUCIDE_SHA256}"   /vendor/lucide.min.js | sha256sum -c -

# --- yt-dlp standalone binary ---
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) asset="yt-dlp_musllinux";         sum="${YTDLP_SHA256_AMD64}" ;; \
      arm64) asset="yt-dlp_musllinux_aarch64"; sum="${YTDLP_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 3 --retry-delay 2 -o /vendor/yt-dlp \
        "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${asset}"; \
    test "$(stat -c%s /vendor/yt-dlp)" -gt 5000000; \
    printf '%s  %s\n' "$sum" /vendor/yt-dlp | sha256sum -c -; \
    chmod 0755 /vendor/yt-dlp

# --- s6-overlay ---
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) s6arch="x86_64";  sum="${S6_SHA256_X86_64}" ;; \
      arm64) s6arch="aarch64"; sum="${S6_SHA256_AARCH64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}"; \
    curl -fsSL --retry 3 --retry-delay 2 -o /tmp/s6-noarch.tar.xz \
        "${base}/s6-overlay-noarch.tar.xz"; \
    curl -fsSL --retry 3 --retry-delay 2 -o /tmp/s6-arch.tar.xz \
        "${base}/s6-overlay-${s6arch}.tar.xz"; \
    test "$(stat -c%s /tmp/s6-noarch.tar.xz)" -gt 1000; \
    test "$(stat -c%s /tmp/s6-arch.tar.xz)"   -gt 100000; \
    printf '%s  %s\n' "${S6_SHA256_NOARCH}" /tmp/s6-noarch.tar.xz | sha256sum -c -; \
    printf '%s  %s\n' "$sum"                /tmp/s6-arch.tar.xz   | sha256sum -c -; \
    mkdir -p /s6root; \
    tar -C /s6root -Jxpf /tmp/s6-noarch.tar.xz; \
    tar -C /s6root -Jxpf /tmp/s6-arch.tar.xz; \
    test -x /s6root/init


# ===========================================================================
# Stage 3 - runtime image
# ===========================================================================
FROM alpine:${ALPINE_VERSION}

# ffmpeg/ffprobe, ImageMagick 7 with its format delegates, and nginx.
#
# ImageMagick delegates are separate packages on Alpine and `magick` fails
# silently-ish without them. WebP/TIFF/JPEG are their own packages; HEIC *and*
# AVIF both go through the imagemagick-heic coder, which links libheif.
#
# On Alpine >= 3.23 libheif is plugin-based: the base package only pulls in
# decoders (dav1d, libde265). Encoders must be requested explicitly -
# libheif-aom for AVIF output, libheif-x265 for HEIC output.
#
# Leave them out and `magick in.png out.avif` exits 0 and writes a PNG with an
# .avif extension. Verified on alpine:3.24 / ImageMagick 7.1.2-27: no error, no
# warning, wrong file. That is why they are pinned here.
#
# libavif is included for its standalone avifenc/avifdec tools; ImageMagick's
# own AVIF path goes through libheif, not libavif.
#
# rsvg-convert renders uploaded SVGs, potrace traces the SVG output that asks
# for outlines. internal/runner/image.go calls both directly: ImageMagick is
# denied the SVG coder in docker/policy.xml.
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        nginx \
        ffmpeg \
        imagemagick \
        imagemagick-heic \
        imagemagick-jpeg \
        imagemagick-webp \
        imagemagick-tiff \
        rsvg-convert \
        potrace \
        libheif-aom \
        libheif-x265 \
        libavif

# s6-overlay puts its own tools in /command; the app resolves yt-dlp, ffmpeg,
# ffprobe and magick through PATH, so /usr/local/bin and /usr/bin must be on it.
ENV PATH="/command:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# s6 behaviour: stop the container if any oneshot fails instead of booting
# into a broken state.
ENV S6_BEHAVIOUR_IF_STAGE2_FAILS=2 \
    S6_VERBOSITY=1 \
    S6_KILL_GRACETIME=10000 \
    S6_SERVICES_GRACETIME=10000

# Application defaults. Every one of these is overridable from compose/.env.
ENV PUID=1000 \
    PGID=1000 \
    TZ=UTC \
    DATA_DIR=/data \
    LISTEN_ADDR=127.0.0.1:1144 \
    XACCEL_PREFIX=/_protected \
    LOG_LEVEL=warn \
    MAX_CONCURRENT_JOBS=2 \
    QUEUE_DEPTH=256 \
    DEFAULT_RETENTION_DAYS=7 \
    MAX_UPLOAD_MIB=4096 \
    SESSION_TTL_HOURS=720

COPY --from=assets /s6root/ /
COPY --from=gobuild /out/aiofiles /usr/local/bin/aiofiles
COPY --from=assets /vendor/yt-dlp /usr/local/bin/yt-dlp

# Static frontend, then the version-pinned vendor bundle it references.
COPY frontend/ /usr/share/nginx/html/
COPY --from=assets /vendor/alpine.min.js /vendor/lucide.min.js /usr/share/nginx/html/vendor/

COPY docker/nginx.conf          /etc/nginx/nginx.conf.template
COPY docker/proxy-headers.conf  /etc/nginx/proxy-headers.conf

# Replaces the permissive stock policy: `magick` chooses its decoder from the
# uploaded bytes, so every coder linked into the binary is reachable from
# arbitrary input unless it is denied here. Path is fixed by the imagemagick
# package's CONFIGURE_PATH (/etc/ImageMagick-7/).
COPY docker/policy.xml          /etc/ImageMagick-7/policy.xml
COPY docker/rootfs/ /

# COPY does not reliably carry the executable bit from every build context
# (Windows checkouts, some CI archives), so set it explicitly. `finish` files
# are read by s6-rc-compile and do not need to be executable.
RUN set -eu; \
    chmod 0755 \
        /usr/local/bin/aiofiles \
        /usr/local/bin/yt-dlp \
        /etc/s6-overlay/scripts/init-aiofiles \
        /etc/s6-overlay/s6-rc.d/aiofiles/run \
        /etc/s6-overlay/s6-rc.d/nginx/run; \
    mkdir -p /data /run/nginx; \
    magick -list policy | grep -q 'Policy: Coder' || \
        { echo "policy.xml is not being read - check CONFIGURE_PATH" >&2; exit 1; }; \
    # Last pattern in the file; missing means the reader stopped early.
    magick -list policy | grep -q 'INLINE' || \
        { echo "policy.xml was only partly parsed - see the header in it" >&2; exit 1; }; \
    rsvg-convert --version >/dev/null

EXPOSE 8000

# nginx is unprivileged and cannot bind :80, so it listens on 8000 and compose
# publishes 1144:8000. Probing through nginx checks both processes at once;
# /api/health is registered outside the auth middleware.
HEALTHCHECK --interval=60s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8000/api/health || exit 1

ENTRYPOINT ["/init"]

LABEL org.opencontainers.image.title="aiofiles" \
      org.opencontainers.image.description="Self-hosted media downloader, converter and compressor" \
      org.opencontainers.image.licenses="GPL-2.0-only" \
      org.opencontainers.image.source="https://github.com/panonim/aiofiles" \
      org.opencontainers.image.url="https://github.com/panonim/aiofiles"
