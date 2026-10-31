// Command fakeytdlp behaves like yt-dlp (and ffprobe/ffmpeg, depending on
// the name it is invoked as) according to a scenario file. It drives
// pipeline and integrity tests without network access (§15).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

func main() {
	switch filepath.Base(os.Args[0]) {
	case "ffprobe":
		os.Exit(ffprobe(os.Args[1:]))
	case "ffmpeg":
		os.Exit(ffmpeg(os.Args[1:]))
	}
	os.Exit(ytdlp(os.Args[1:]))
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", a...)
	return 1
}

func has(args []string, flag string) bool { return slices.Contains(args, flag) }

func value(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
	}
	return ""
}

func values(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func impersonating(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--impersonate") {
			return true
		}
	}
	return false
}

func ownVersion(sc *scenario.Scenario) string {
	exe, _ := os.Executable()
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(os.Args[0]), "version")); err == nil {
		return strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "version")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return sc.Version
}

// attempt increments and returns the attempt counter for key.
func attempt(key string) int {
	dir := os.Getenv(scenario.EnvState)
	if dir == "" {
		return 1
	}
	p := filepath.Join(dir, strings.NewReplacer("/", "_", ":", "_").Replace(key))
	n := 0
	if b, err := os.ReadFile(p); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	os.WriteFile(p, []byte(strconv.Itoa(n)), 0o644)
	return n
}

func pick(bs []scenario.Behavior, n int) scenario.Behavior {
	if len(bs) == 0 {
		return scenario.Behavior{}
	}
	if n > len(bs) {
		n = len(bs)
	}
	return bs[n-1]
}

// apply runs the failure part of a behavior. It returns (exit, true) if the
// call must end now.
func apply(sc *scenario.Scenario, b scenario.Behavior, args []string) (int, bool) {
	if b.SleepMs > 0 {
		time.Sleep(time.Duration(b.SleepMs) * time.Millisecond)
	}
	if b.Hang {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Fprintln(os.Stderr, "ERROR: Interrupted by user")
		return 1, true
	}
	if b.RequireImpersonate && !impersonating(args) {
		fmt.Fprintln(os.Stderr, "ERROR: [fake] x1: Unable to download webpage: HTTP Error 403: Forbidden (caused by <HTTPError 403: Forbidden>)")
		return 1, true
	}
	if b.RequireVersion != "" && ownVersion(sc) != b.RequireVersion {
		fmt.Fprintln(os.Stderr, "ERROR: [fake] x1: Unable to extract video data; please report this issue on <issues>, filling out the appropriate issue template. Confirm you are on the latest version using  yt-dlp -U")
		return 1, true
	}
	if b.Traceback {
		fmt.Fprintln(os.Stderr, "Traceback (most recent call last):")
		fmt.Fprintln(os.Stderr, `  File "yt_dlp/__main__.py", line 1, in <module>`)
		fmt.Fprintln(os.Stderr, "TypeError: argument of type 'NoneType' is not iterable")
		return 1, true
	}
	for _, l := range b.Stderr {
		fmt.Fprintln(os.Stderr, l)
	}
	if b.Exit != 0 {
		return b.Exit, true
	}
	return 0, false
}

func videoJSON(v scenario.Video) map[string]any {
	formats := []map[string]any{}
	if !v.AudioOnly {
		formats = append(formats, map[string]any{"format_id": "137", "vcodec": "avc1", "acodec": "none", "filesize": 1000})
	}
	langs := v.Languages
	if len(langs) == 0 {
		langs = []string{""}
	}
	for i, l := range langs {
		f := map[string]any{"format_id": fmt.Sprintf("251-%d", i), "vcodec": "none", "acodec": "opus", "abr": 128.0, "filesize": 100}
		if l != "" {
			f["language"] = l
		}
		formats = append(formats, f)
	}
	subs := map[string]any{}
	for _, l := range v.Subtitles {
		subs[l] = []map[string]any{{"ext": "vtt", "url": "https://fake.test/sub/" + l}}
	}
	date := v.UploadDate
	if date == "" {
		date = "20240102"
	}
	m := map[string]any{
		"id": v.ID, "title": v.Title, "fulltitle": v.Title, "extractor": "fake", "extractor_key": "Fake",
		"webpage_url": scenario.VideoPrefix + v.ID, "webpage_url_domain": "fake.test", "duration": v.Duration,
		"uploader": v.Uploader, "upload_date": date, "formats": formats, "subtitles": subs,
		"http_headers": map[string]string{"User-Agent": "FakeUA"}, "_type": "video",
	}
	if v.ChannelURL != "" {
		m["channel_url"] = v.ChannelURL
	}
	if v.ExtractorKey != "" {
		m["extractor_key"], m["extractor"] = v.ExtractorKey, strings.ToLower(v.ExtractorKey)
	}
	if v.SizeBytes > 0 {
		m["filesize_approx"] = v.SizeBytes
	}
	if v.Thumbnail != "" {
		m["thumbnail"] = v.Thumbnail
	}
	if v.Thumbnails != nil {
		m["thumbnails"] = v.Thumbnails
	}
	return m
}

func playlistJSON(sc *scenario.Scenario, pl scenario.Playlist, ids []string) map[string]any {
	entries := []map[string]any{}
	for _, spec := range ids {
		id, overrides, _ := strings.Cut(spec, "|")
		if sub, ok := sc.Playlists[id]; ok {
			entries = append(entries, map[string]any{"_type": "url", "ie_key": "FakePlaylist", "id": sub.ID,
				"url": scenario.PlaylistPrefix + sub.ID, "title": sub.Title})
			continue
		}
		v, ok := sc.Videos[id]
		if !ok {
			v = scenario.Video{ID: id, Title: id}
		}
		e := map[string]any{"_type": "url", "ie_key": "Fake", "id": id, "url": scenario.VideoPrefix + id, "title": v.Title}
		if pl.Resolved {
			e = map[string]any{"id": id, "extractor_key": "Fake", "title": v.Title, "webpage_url": scenario.VideoPrefix + id,
				"url": "https://fake.test/media/" + id + ".mp4", "format_id": "hd", "ext": "mp4"}
		}
		if v.Duration > 0 {
			e["duration"] = v.Duration
		}
		if v.Timestamp > 0 {
			e["timestamp"] = v.Timestamp
		}
		if v.UploadDate != "" {
			e["upload_date"] = v.UploadDate
		}
		if v.LiveStatus != "" {
			e["live_status"] = v.LiveStatus
		}
		if v.Availability != "" {
			e["availability"] = v.Availability
		}
		for _, kv := range strings.Split(overrides, "|") {
			if k, val, ok := strings.Cut(kv, "="); ok {
				if val == "" {
					delete(e, k)
				} else {
					e[k] = val
				}
			}
		}
		entries = append(entries, e)
	}
	key := pl.ExtractorKey
	if key == "" {
		key = "FakePlaylist"
	}
	m := map[string]any{"_type": "playlist", "id": pl.ID, "title": pl.Title, "entries": entries, "extractor_key": key,
		"thumbnails": []map[string]any{{"url": "https://fake.test/img/" + pl.ID + ".jpg", "width": 100}}}
	if pl.ChannelID != "" {
		m["channel_id"] = pl.ChannelID
	}
	if pl.Uploader != "" {
		m["uploader"], m["channel"] = pl.Uploader, pl.Uploader
	}
	if pl.Count > 0 {
		m["playlist_count"] = pl.Count
	}
	return m
}

// window applies "-I :N" (the only item selection the server uses).
func window(args []string, ids []string) []string {
	if n, err := strconv.Atoi(strings.TrimPrefix(value(args, "-I"), ":")); err == nil && n < len(ids) {
		return ids[:n]
	}
	return ids
}

func ytdlp(args []string) int {
	sc, err := scenario.Load(os.Getenv(scenario.EnvScenario))
	if err != nil {
		return fail("fake: no scenario: %v", err)
	}
	if has(args, "--version") {
		fmt.Println(ownVersion(sc))
		return 0
	}
	if has(args, "--list-impersonate-targets") {
		fmt.Println("[info] Available impersonate targets")
		fmt.Println("Client  OS  Source")
		fmt.Println("--------------------")
		for _, t := range sc.Targets {
			fmt.Println(t + " curl_cffi")
		}
		return 0
	}
	url := ""
	if i := slices.Index(args, "--"); i >= 0 && i+1 < len(args) {
		url = args[i+1]
	}
	if alias, ok := sc.Aliases[url]; ok {
		url = alias
	}
	if info := value(args, "--load-info-json"); info != "" {
		raw, err := os.ReadFile(info)
		if err != nil {
			return fail("fake: cannot read info json: %v", err)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		id, _ := m["id"].(string)
		v, ok := sc.Videos[id]
		if !ok {
			return fail("fake: unknown video %q", id)
		}
		if has(args, "--skip-download") {
			return subtitles(sc, v, args)
		}
		return download(sc, v, args)
	}
	if !has(args, "-J") {
		return fail("fake: unsupported invocation %v", args)
	}
	kind := "probe"
	if has(args, "--no-playlist") {
		kind = "meta"
	}
	if id, ok := strings.CutPrefix(url, scenario.PlaylistPrefix); ok {
		pl, ok := sc.Playlists[id]
		if !ok {
			return fail("[fake] %s: Unable to download webpage: HTTP Error 404: Not Found", id)
		}
		if code, stop := apply(sc, pick(pl.Probe, attempt(kind+"-pl-"+id)), args); stop {
			return code
		}
		ids := pl.Entries
		if len(pl.Listings) > 0 {
			n := min(attempt("listing-pl-"+id), len(pl.Listings))
			ids = pl.Listings[n-1]
		}
		m := playlistJSON(sc, pl, window(args, ids))
		m["webpage_url"] = url
		return printJSON(m)
	}
	id, ok := strings.CutPrefix(url, scenario.VideoPrefix)
	v, known := sc.Videos[id]
	if !ok || !known {
		return fail("Unsupported URL: %s", url)
	}
	bs := v.Probe
	if kind == "meta" {
		bs = v.Meta
	}
	if code, stop := apply(sc, pick(bs, attempt(kind+"-"+id)), args); stop {
		return code
	}
	return printJSON(videoJSON(v))
}

func printJSON(v any) int {
	b, _ := json.Marshal(v)
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
	return 0
}

func homeDir(args []string) string {
	for _, p := range values(args, "-P") {
		if h, ok := strings.CutPrefix(p, "home:"); ok {
			return h
		}
	}
	return "."
}

func tempDir(args []string) string {
	for _, p := range values(args, "-P") {
		if h, ok := strings.CutPrefix(p, "temp:"); ok {
			return h
		}
	}
	return homeDir(args)
}

func download(sc *scenario.Scenario, v scenario.Video, args []string) int {
	b := pick(v.Download, attempt("download-"+v.ID))
	home, tmp := homeDir(args), tempDir(args)
	os.MkdirAll(tmp, 0o755)
	if b.WritePart {
		os.WriteFile(filepath.Join(tmp, "media.f137.mp4.part"), []byte("partial"), 0o644)
	}
	if b.Hang {
		fmt.Println(`@@P {"status": "downloading", "downloaded_bytes": 10, "total_bytes": 100, "speed": 5.0, "eta": 18}`)
	}
	if code, stop := apply(sc, b, args); stop {
		return code
	}
	fmt.Printf("[info] %s: Downloading 1 format(s): %s\n", v.ID, value(args, "-f"))
	for _, pct := range []int{10, 60, 100} {
		status := "downloading"
		if pct == 100 {
			status = "finished"
		}
		fmt.Printf("@@P {\"status\": %q, \"downloaded_bytes\": %d, \"total_bytes\": 100, \"speed\": 1000.0, \"eta\": 1, \"filename\": \"%s\"}\n",
			status, pct, filepath.Join(tmp, "media.f137.mp4"))
	}
	os.Remove(filepath.Join(tmp, "media.f137.mp4.part"))
	if b.NoMedia {
		return 0
	}
	ext := value(args, "--merge-output-format")
	if ext == "" {
		ext = "mkv"
	}
	if v.Single != "" {
		ext = v.Single
	}
	info := scenario.MediaInfo{FakeMedia: true, Duration: v.Duration, Video: 1, Audio: max(1, len(v.Languages))}
	if v.AudioOnly {
		info.Video = 0
	}
	if b.BadMedia {
		info.Video, info.Duration = 0, 0
	}
	info.Pad = strings.Repeat("x", 64)
	data, _ := json.Marshal(info)
	media := filepath.Join(home, "media."+ext)
	if err := os.WriteFile(media, data, 0o644); err != nil {
		return fail("fake: %v", err)
	}
	formatID := "137+251-0"
	if v.Single == "" {
		fmt.Printf("[Merger] Merging formats into \"%s\"\n", media)
	} else if to := value(args, "--remux-video"); to != "" && to != ext {
		remuxed := filepath.Join(home, "media."+to)
		fmt.Printf("[VideoRemuxer] Remuxing video from %s to %s; Destination: %s\n", ext, to, remuxed)
		if err := os.Rename(media, remuxed); err != nil {
			return fail("fake: %v", err)
		}
		media = remuxed
	}
	if len(v.Languages) > 1 {
		ids := []string{"137"}
		for i := range v.Languages {
			ids = append(ids, fmt.Sprintf("251-%d", i))
		}
		formatID = strings.Join(ids, "+")
	}
	if v.AudioOnly {
		formatID = "251-0"
	}
	if v.Single != "" {
		formatID = "18"
	}
	for i, a := range args {
		if a == "--print-to-file" && i+2 < len(args) {
			f, err := os.OpenFile(args[i+2], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err == nil {
				fmt.Fprintf(f, "%s\t%s\n", media, formatID)
				f.Close()
			}
		}
	}
	return 0
}

func subtitles(sc *scenario.Scenario, v scenario.Video, args []string) int {
	b := pick(v.Subs, attempt("subs-"+v.ID))
	if code, stop := apply(sc, b, args); stop {
		return code
	}
	home := homeDir(args)
	ext := "vtt"
	if value(args, "--convert-subs") == "srt" {
		ext = "srt"
	}
	for _, l := range v.Subtitles {
		os.WriteFile(filepath.Join(home, "media."+l+"."+ext), []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n"), 0o644)
	}
	return 0
}

func ffprobe(args []string) int {
	if len(args) == 0 {
		return 1
	}
	if has(args, "-version") {
		fmt.Println("ffprobe version fake")
		return 0
	}
	data, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var info scenario.MediaInfo
	if json.Unmarshal(data, &info) != nil || !info.FakeMedia {
		fmt.Fprintln(os.Stderr, "Invalid data found when processing input")
		return 1
	}
	var streams []map[string]any
	for i := 0; i < info.Video; i++ {
		streams = append(streams, map[string]any{"codec_type": "video"})
	}
	for i := 0; i < info.Audio; i++ {
		streams = append(streams, map[string]any{"codec_type": "audio"})
	}
	out := map[string]any{"streams": streams, "format": map[string]any{"duration": strconv.FormatFloat(info.Duration, 'f', 3, 64)}}
	b, _ := json.Marshal(out)
	os.Stdout.Write(b)
	return 0
}

// ffmpeg writes a small JPEG to its last argument (frame extraction,
// thumbnail conversion). FAKE_FFMPEG_FAIL=1 makes it fail.
func ffmpeg(args []string) int {
	if os.Getenv("FAKE_FFMPEG_FAIL") == "1" || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake ffmpeg failure")
		return 1
	}
	if has(args, "-version") {
		fmt.Println("ffmpeg version fake")
		return 0
	}
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	if err := os.WriteFile(args[len(args)-1], buf.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
