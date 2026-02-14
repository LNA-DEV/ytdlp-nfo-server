#!/bin/sh
CHANNEL="${YTDLP_CHANNEL:-stable}"
case "$CHANNEL" in
  nightly)
    pip install -U https://github.com/yt-dlp/yt-dlp-nightly-builds/releases/latest/download/yt-dlp.tar.gz || true
    ;;
  master)
    pip install -U https://github.com/yt-dlp/yt-dlp-master-builds/releases/latest/download/yt-dlp.tar.gz || true
    ;;
  *)
    pip install -U yt-dlp || true
    ;;
esac
exec ytdlp-nfo-server
