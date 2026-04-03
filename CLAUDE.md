# CLAUDE.md

## Project Priorities

- **File integrity is the #1 priority.** Files in the output directory must always be complete and intact. Never move or copy files that are still being written to.

## Architecture

- Go server in `server/` that wraps `ytdlp-nfo` (a yt-dlp wrapper) for downloading media
- Each download job runs in an isolated subdirectory (`downloadDir/{jobID}/`) to prevent cross-contamination between concurrent downloads
- Completed files are moved to `outputDir` via a staging directory for atomicity
- Shared download archive (`.ytdlp-archive.txt`) lives in the root `downloadDir`

## Build & Run

```bash
cd server && go build
```

## Key Files

- `server/download.go` — Download manager, job lifecycle, file movement
- `server/handlers.go` — HTTP API handlers
- `server/persist.go` — Job state persistence
- `server/main.go` — Entry point and configuration
