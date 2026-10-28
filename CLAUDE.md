# CLAUDE.md

## Project Priorities

- **File integrity is the #1 priority.** Files in the output directory must always be complete and intact. Never move or copy files that are still being written to.
- The invariants in `docs/overhaul-concept.md` §4.5 are tested by `server/integrity_test.go` (SIGKILL at every pipeline step and at both points of a subscription check), `server/reddit_integrity_test.go` (SIGKILL during a Reddit gallery and around a duplicate skip) and `internal/library` (fault injection). Keep them green when touching finalize, recovery or the pipeline:
  1. nothing appears in the library before it passed verify;
  2. an item appears through one atomic rename (or `renameat2` exchange) of a fully written, fsynced folder;
  3. existing library content is never deleted before its replacement is in place;
  4. an archive entry exists if and only if the item was finalized;
  5. every step is idempotent and resumable after a crash.

## Architecture

- Go server in `server/`. It calls the `yt-dlp` standalone binary directly (`ytdlp-nfo` is no longer a runtime dependency) and does probing, format selection, NFO, thumbnails, archive and the final move itself.
- A **job** is one submission; probing creates one **item** per video. Items run sequentially within a job; jobs run in parallel up to `MAX_CONCURRENT` (plus per-site limits).
- Pipeline per item: fetch meta (`info.json`) → select formats → download media only → `ffprobe` verify → extras (thumbnail, subtitles; best-effort, warnings only) → NFO → finalize.
- Each item has its own work dir `DOWNLOAD_DIR/.work/<job>/<item>/`. Finalize stages the folder in `<library>/.staging/<job>-<item>/`, writes markers, then renames/exchanges it into place; the plan is stored in the DB before any file is touched.
- The archive lives in SQLite and is written only after finalize; `DOWNLOAD_DIR/.ytdlp-archive.txt` is imported at start and kept in sync for other tools. yt-dlp's own `--download-archive` is not used.
- Errors are classified (`internal/ytdlp/classify.go`) and the retry policy (`internal/scheduler/policy.go`) decides per item; a single scheduler goroutine owns all slots.
- yt-dlp versions are managed in `DATA_DIR/ytdlp/<channel>/<tag>/` (SHA-256 verified, smoke-tested); verified stable/nightly builds are bundled in the image under `/opt/ytdlp/bundled`.
- Every yt-dlp call passes `--ignore-config` and puts the URL after `--`. Never add free-form yt-dlp arguments or `--compat-options allow-unsafe-ext`.
- Subscriptions (`docs/subscriptions-concept.md`) only create ordinary jobs with pre-planned items; they never touch the library or the archive. `internal/subscriptions` runs the check loop (one check per site, cooldowns, windowed listings) and the pure diff `Apply`; the manager commits a check (entries, jobs, entry links, check state) in one transaction. Entry status is derived in SQL, never stored. Planned jobs (`Job.Planned()`, empty `norm_url`) never probe or expand.
- Reddit (`docs/reddit-concept.md`): `internal/reddit` holds URLs, the post model and the provider chain; `native` parses logged-out pages through the browser-identity client (`browser`, azuretls), `arcticshift` calls the archive API. Subscriptions list Reddit sources through `subscriptions.RedditLister`; items whose URL is a Reddit post run `pipeline.attemptReddit` (post.json, media keys, image verify by content, `.ytdlp-nfo.json` marker, media index for duplicates). Only `reddit <id>` video keys go to `.ytdlp-archive.txt` (`library.FileKey`). Reddit and media hosts are only reached through the browser client's host allowlist.
- The scheduler has three bands (interactive > auto > backlog); background jobs share `backgroundSlots` and yield between two items (`Scheduler.ShouldYield`).

## Build & Test

```bash
cd server
go build
go vet ./...
go test -race ./...          # includes fake yt-dlp scenarios and SIGKILL integrity tests
docker build -t ytdlp-nfo-server .
```

Tests never touch the network: `internal/testutil/fakereddit` serves a fake Reddit, Arctic Shift and i.redd.it over TLS with a test CA; `internal/testutil/fakeytdlp` is a fake yt-dlp (also `ffprobe`/`ffmpeg` by name) driven by `internal/testutil/scenario` files. Failpoints (`internal/failpoint`, env `YTDLP_NFO_FAILPOINTS`) kill the process at named steps.

## Workflow

- Work stage by stage (for a concept: its rollout phases, split into steps that build on each other). Finish and test each stage before starting the next one: `go build`, `go vet ./...` and `go test -race ./...` must pass, plus new tests for what the stage adds.
- Commit at sensible intervals: one commit per finished and tested stage or step, never a broken or half-done state. Push the development branch after each phase.

## Key Files

- `server/main.go` — entry point, signals, `healthcheck`, `bundle` and `reddit-probe` subcommands
- `server/internal/manager/` — wiring, v1 migration, crash recovery, job operations, events
- `server/internal/pipeline/` — probe, plan, per-item steps, finalize, extras, verify
- `server/internal/library/` — naming (yt-dlp-compatible sanitizing), atomic finalize, archive file
- `server/internal/scheduler/` — slots, queue bands, cooldowns, retry policy
- `server/internal/subscriptions/` — subscription check loop, listing, diff (`Apply`), filters, canonical URLs
- `server/internal/reddit/` — Reddit URLs, posts, provider chain; `native/`, `arcticshift/`, `browser/` (browser identity), `jschallenge/` (answers Reddit's JavaScript check in goja, in a child process of the server binary: `main` and every test binary that runs it handle `jschallenge.ChildEnv` first), `probe/` (`reddit-probe`)
- `server/internal/ytdlp/` — command lines, runner, progress parser, error classifier; `versions/` is the version manager
- `server/internal/store/` — SQLite schema, migrations, jobs.json/archive import
- `server/internal/nfo/` — NFO writer (golden files from the Python implementation in `testdata/`)
- `server/internal/api/` — `/api/v1`, legacy `/api` routes for the browser add-on, auth, SSE
- `server/web/static/` — web UI (plain ES modules, embedded, no build step)
- `browser-addon/` — browser extension (must keep working with `POST /api/download` → 201/409)
- `docs/overhaul-concept.md` — design and decisions; `docs/subscriptions-concept.md` — subscriptions
