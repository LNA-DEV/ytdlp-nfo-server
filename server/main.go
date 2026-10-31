package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/api"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/privdrop"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/jschallenge"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/probe"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
	"github.com/LNA-DEV/ytdlp-nfo-server/web"
)

func main() {
	if os.Getenv(jschallenge.ChildEnv) != "" { // answering one Reddit check, nothing else
		os.Exit(jschallenge.ChildMain(os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "bundle":
			os.Exit(bundle(os.Args[2:]))
		case "reddit-probe":
			os.Exit(probe.Main(os.Args[2:], os.Stdout))
		case "version", "--version", "-v":
			fmt.Println("ytdlp-nfo-server " + buildinfo.String())
			return
		}
	}
	if err := run(nil); err != nil {
		log.Fatal(err)
	}
}

// run runs the server until SIGTERM or SIGINT. tweak adjusts the manager
// options (tests: a fake Reddit).
func run(tweak func(*manager.Options)) error {
	env, err := config.LoadEnv()
	if err != nil {
		return err
	}
	log.Printf("ytdlp-nfo-server %s", buildinfo.String())

	var recursive []string
	if env.DataDir != "" {
		recursive = append(recursive, env.DataDir)
	}
	recursive = append(recursive, env.WorkRoot(), filepath.Join(env.LibraryRoot(), ".staging"), filepath.Join(env.LibraryRoot(), ".trash"))
	shallow := []string{env.DownloadDir, env.LibraryRoot()}
	for _, l := range env.Libraries {
		shallow = append(shallow, l.Path)
	}
	if err := privdrop.Apply(privdrop.Config{UID: env.PUID, GID: env.PGID, Umask: env.Umask, Recursive: recursive, Shallow: shallow}, log.Printf); err != nil {
		return err
	}

	opts := manager.Options{Env: env}
	if tweak != nil {
		tweak(&opts)
	}
	m, err := manager.New(opts)
	if err != nil {
		return err
	}
	if env.DataDir == "" {
		log.Printf("DATA_DIR is not set: using an in-memory database (nothing survives a restart)")
	}

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	if err := m.Start(baseCtx); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              ":" + env.Port,
		Handler:           api.New(m, web.Static()),
		ReadHeaderTimeout: 15 * time.Second,
		// Long-lived SSE requests end when the base context is cancelled.
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on :%s (downloads: %s, library: %s)", env.Port, env.DownloadDir, env.LibraryRoot())
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case s := <-sig:
		log.Printf("received %s, shutting down", s)
	case err := <-errCh:
		return err
	}

	// 1. Stop accepting requests; end event streams.
	cancelBase()
	httpCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(httpCtx)
	cancel()
	// 2. Interrupt downloads (they resume after the next start) and close
	// the database. yt-dlp gets SIGINT and up to 15 s to exit.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("shutdown complete")
	return nil
}

// healthcheck is used by the Docker HEALTHCHECK.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, resp.Status)
		return 1
	}
	return 0
}

// bundle downloads verified yt-dlp builds into a directory at image build
// time, so the container works offline on first boot (§6.2).
func bundle(args []string) int {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	dest := fs.String("dest", "/opt/ytdlp/bundled", "destination directory")
	arch := fs.String("arch", runtime.GOARCH, "target architecture (GOARCH)")
	channels := fs.String("channels", "stable,nightly", "channels to bundle")
	tags := fs.String("tags", "", "optional pinned tags, e.g. stable=2026.08.19,nightly=2026.10.10.233141")
	fs.Parse(args)

	pinned := map[string]string{}
	for _, kv := range strings.Split(*tags, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			pinned[k] = v
		}
	}
	f := &versions.Fetcher{Client: &http.Client{Timeout: 10 * time.Minute}, Token: os.Getenv("GITHUB_TOKEN")}
	ctx := context.Background()
	for _, ch := range strings.Split(*channels, ",") {
		ch = strings.TrimSpace(ch)
		if !config.ValidChannel(ch) {
			fmt.Fprintf(os.Stderr, "unknown channel %q\n", ch)
			return 2
		}
		tag := pinned[ch]
		if tag == "" {
			var err error
			if tag, err = f.LatestTag(ctx, ch); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", ch, err)
				return 1
			}
		}
		dir := filepath.Join(*dest, ch, tag)
		os.RemoveAll(dir)
		m, err := f.Fetch(ctx, ch, tag, *arch, dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", ch, tag, err)
			return 1
		}
		fmt.Printf("bundled %s %s (%s, sha256 %s)\n", ch, tag, m.Asset, m.SHA256)
	}
	return 0
}
