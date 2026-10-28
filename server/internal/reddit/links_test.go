package reddit

import (
	"context"
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

func TestResolveLinks(t *testing.T) {
	l := Links{Hosts: append(config.DefaultRedditExternalHosts(), "*.example.org")}
	ctx := context.Background()
	for raw, want := range map[string]LinkTarget{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10":   {URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Key: "youtube dQw4w9WgXcQ"},
		"https://youtu.be/dQw4w9WgXcQ?si=x":                  {URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Key: "youtube dQw4w9WgXcQ"},
		"https://m.youtube.com/shorts/dQw4w9WgXcQ":           {URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Key: "youtube dQw4w9WgXcQ"},
		"https://www.redgifs.com/watch/HappyLoudFox":         {URL: "https://www.redgifs.com/watch/happyloudfox", Key: "redgifs happyloudfox"},
		"https://redgifs.com/ifr/happyloudfox":               {URL: "https://www.redgifs.com/watch/happyloudfox", Key: "redgifs happyloudfox"},
		"https://v3.redgifs.com/watch/happyloudfox#rel=user": {URL: "https://www.redgifs.com/watch/happyloudfox", Key: "redgifs happyloudfox"},
		"https://streamable.com/e/abc123":                    {URL: "https://streamable.com/abc123", Key: "streamable abc123"},
		"http://i.imgur.com/AbCdE12.gifv":                    {URL: "https://imgur.com/AbCdE12", Key: "imgur AbCdE12"},
		"https://imgur.com/AbCdE12":                          {URL: "https://imgur.com/AbCdE12", Key: "imgur AbCdE12"},
		"https://i.imgur.com/AbCdE12.JPG":                    {URL: "https://i.imgur.com/AbCdE12.jpg", Key: "imgur AbCdE12", Image: true},
		"https://media.example.org/v/1?x=2":                  {URL: "https://media.example.org/v/1?x=2", Key: "url https://media.example.org/v/1?x=2"},
	} {
		got, ok := l.Resolve(ctx, raw)
		if !ok || got.URL != want.URL || got.Key != want.Key || got.Image != want.Image {
			t.Errorf("%s: %+v %v, want %+v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"https://imgur.com/a/AbCdE12",                       // album
		"https://imgur.com/gallery/AbCdE12",                 // gallery
		"https://www.youtube.com/watch?v=short",             // bad id
		"https://www.youtube.com/@channel",                  // not a video
		"https://vimeo.com/123456",                          // not on the list
		"https://www.reddit.com/r/pics/comments/abc/x/",     // Reddit itself
		"http://media.example.org/v/1",                      // plain HTTP without a known form
		"https://media.example.org:8443/v/1",                // other port
		"https://user@media.example.org/v/1",                // credentials
		"javascript:alert(1)",                               // not a URL to follow
		"https://youtube.com.evil.test/watch?v=dQw4w9WgXcQ", // look-alike host
	} {
		if got, ok := l.Resolve(ctx, raw); ok {
			t.Errorf("%s followed: %+v", raw, got)
		}
	}
	// A host without a known form must resolve to public addresses.
	l.Public = func(context.Context, string) bool { return false }
	if _, ok := l.Resolve(ctx, "https://media.example.org/v/1"); ok {
		t.Fatal("private host followed")
	}
	if _, ok := l.Resolve(ctx, "https://youtu.be/dQw4w9WgXcQ"); !ok {
		t.Fatal("known forms need no address check")
	}
}
