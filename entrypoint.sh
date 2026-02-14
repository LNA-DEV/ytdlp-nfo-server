#!/bin/sh
yt-dlp --update || true
exec ytdlp-nfo-server
