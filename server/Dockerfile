# syntax=docker/dockerfile:1

# --- build: server binary for the target platform, plus verified yt-dlp
# builds for the target architecture (fetched natively on the build host).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=
# Optional pins, e.g. "stable=2026.08.19,nightly=2026.10.10.233141".
# Empty: the latest stable and nightly at build time.
ARG YTDLP_TAGS=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w \
        -X github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo.Version=${VERSION} \
        -X github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo.Date=${BUILD_DATE}" \
      -o /out/ytdlp-nfo-server . \
 && CGO_ENABLED=0 go run . bundle --dest /out/bundled --arch "$TARGETARCH" --channels stable,nightly --tags "$YTDLP_TAGS"

FROM denoland/deno:bin-2.9.7@sha256:bc5aa4466e21b6d3021226a85ba2e1911f7c386254d97b9d797903ab74edace2 AS deno

# --- runtime: glibc for the yt-dlp standalone builds; no Python, pip or Git.
FROM debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*
COPY --from=deno /deno /usr/local/bin/deno
COPY --from=build /out/bundled /opt/ytdlp/bundled
COPY --from=build /out/ytdlp-nfo-server /usr/local/bin/ytdlp-nfo-server
ENV PORT=8080 \
    DOWNLOAD_DIR=/downloads \
    DATA_DIR=/data \
    YTDLP_BUNDLED_DIR=/opt/ytdlp/bundled
VOLUME ["/downloads", "/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD ["ytdlp-nfo-server", "healthcheck"]
ENTRYPOINT ["tini", "--", "ytdlp-nfo-server"]
