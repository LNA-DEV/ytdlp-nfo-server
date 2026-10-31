package reddit

import "testing"

func TestParseAndCanonical(t *testing.T) {
	cases := []struct {
		in, canonical, name string
		kind                TargetKind
	}{
		{"https://www.reddit.com/r/EarthPorn/", "https://www.reddit.com/r/earthporn/new", "r/EarthPorn", TargetSubreddit},
		{"https://old.reddit.com/r/EarthPorn/new/", "https://www.reddit.com/r/earthporn/new", "r/EarthPorn", TargetSubreddit},
		{"https://reddit.com/r/pics/top/?t=week", "https://www.reddit.com/r/pics/top?t=week", "r/pics", TargetSubreddit},
		{"https://www.reddit.com/r/pics/top/", "https://www.reddit.com/r/pics/top?t=day", "r/pics", TargetSubreddit},
		{"https://np.reddit.com/r/pics/hot", "https://www.reddit.com/r/pics/hot", "r/pics", TargetSubreddit},
		{"https://www.reddit.com/user/Some_User/", "https://www.reddit.com/user/Some_User/submitted", "u/Some_User", TargetUser},
		{"https://www.reddit.com/u/Some_User/submitted/?sort=top&t=all", "https://www.reddit.com/user/Some_User/submitted?sort=top&t=all", "u/Some_User", TargetUser},
		{"https://www.reddit.com/r/pics/comments/1AbC23/some_title/", "https://www.reddit.com/r/pics/comments/1abc23/", "r/pics", TargetPost},
		{"https://redd.it/1abc23", "https://www.reddit.com/comments/1abc23/", "", TargetPost},
		{"https://www.reddit.com/gallery/1abc23", "https://www.reddit.com/comments/1abc23/", "", TargetPost},
		{"https://www.reddit.com/r/pics/s/AbCdEf", "https://www.reddit.com/r/pics/s/AbCdEf", "r/pics", TargetShare},
	}
	for _, c := range cases {
		tg, ok := Parse(c.in)
		if !ok || tg.Kind != c.kind || tg.Canonical() != c.canonical || tg.Name() != c.name {
			t.Errorf("%s: ok=%v %+v canonical=%s name=%s", c.in, ok, tg, tg.Canonical(), tg.Name())
		}
	}
	for _, in := range []string{"https://i.redd.it/abc.jpg", "https://v.redd.it/abc", "https://www.youtube.com/r/x",
		"https://www.reddit.com/", "https://www.reddit.com/r/pics/about", "ftp://reddit.com/r/x", "https://www.reddit.com/r/-x/"} {
		if tg, ok := Parse(in); ok {
			t.Errorf("%s parsed as %+v", in, tg)
		}
	}
	if tg, _ := Parse("https://www.reddit.com/user/x_y/"); tg.Folder() != "u_x_y" {
		t.Errorf("user folder %q", tg.Folder())
	}
}

func TestRSSListing(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.reddit.com/r/EarthPorn/.rss":            "https://www.reddit.com/r/EarthPorn/new",
		"https://www.reddit.com/r/EarthPorn/new/.rss":        "https://www.reddit.com/r/EarthPorn/new",
		"https://www.reddit.com/r/EarthPorn/top/.rss?t=week": "https://www.reddit.com/r/EarthPorn/top?t=week",
		"https://www.reddit.com/user/someone/submitted/.rss": "https://www.reddit.com/user/someone/submitted",
		"https://www.reddit.com/r/EarthPorn.rss":             "https://www.reddit.com/r/EarthPorn/new",
	} {
		if got, ok := RSSListing(in); !ok || got != want {
			t.Errorf("%s: %q %v", in, got, ok)
		}
	}
	if _, ok := RSSListing("https://www.youtube.com/feeds/videos.xml?channel_id=x"); ok {
		t.Error("YouTube feed mapped")
	}
}

func TestMediaKeys(t *testing.T) {
	img := Post{ID: "p1"}
	img.Classify("https://i.redd.it/q8x2v0f3k1.jpeg", "image")
	if img.Kind != KindImage || img.MediaKey() != "redditmedia q8x2v0f3k1" || img.Media[0].URL != "https://i.redd.it/q8x2v0f3k1.jpeg" {
		t.Fatalf("image: %+v %s", img, img.MediaKey())
	}
	gif := Post{ID: "p2"}
	gif.Classify("https://i.redd.it/anim.gif", "")
	if gif.Kind != KindGIF || gif.MediaKey() != "redditmedia anim" {
		t.Fatalf("gif: %+v", gif)
	}
	vid := Post{ID: "p3"}
	vid.Classify("https://v.redd.it/AbC123", "video")
	if vid.Kind != KindVideo || vid.MediaKey() != "reddit abc123" {
		t.Fatalf("video: %+v %s", vid, vid.MediaKey())
	}
	gal := Post{ID: "p4"}
	gal.Classify("https://www.reddit.com/gallery/p4", "")
	if gal.Kind != KindGallery || gal.MediaKey() != "" {
		t.Fatalf("gallery without media: %+v", gal)
	}
	gal.Media = []Media{{MediaID: "a"}, {MediaID: "b"}}
	if e, id := gal.ListingKey(); e != ExtractorGallery || len(id) != 16 || GalleryHash([]Media{{MediaID: "b"}, {MediaID: "a"}}) == id {
		t.Fatalf("gallery key: %s %s", e, id)
	}
	txt := Post{ID: "p5"}
	txt.Classify("https://www.reddit.com/r/x/comments/p5/title/", "")
	if txt.Kind != KindText {
		t.Fatalf("self post: %+v", txt)
	}
	link := Post{ID: "p6"}
	link.Classify("https://www.youtube.com/watch?v=x", "link")
	if e, id := link.ListingKey(); link.Kind != KindLink || link.LinkHost() != "youtube.com" || e != ExtractorPost || id != "p6" {
		t.Fatalf("link: %+v", link)
	}
	xp := Post{ID: "p7"}
	xp.Classify("https://www.reddit.com/r/other/comments/orig1/title/", "crosspost")
	if xp.Kind != "" || xp.CrosspostParent != "orig1" {
		t.Fatalf("crosspost: %+v", xp)
	}
}

// i.redd.it serves a file only under its own extension (.jpeg is not .jpg).
func TestImageExtensionsAreKept(t *testing.T) {
	for in, want := range map[string]string{
		"https://i.redd.it/abc.jpeg":           "https://i.redd.it/abc.jpeg",
		"https://i.redd.it/abc.JPG":            "https://i.redd.it/abc.jpg",
		"https://preview.redd.it/abc.jpeg?w=1": "https://i.redd.it/abc.jpeg",
		"https://i.redd.it/abc.png":            "https://i.redd.it/abc.png",
		// A preview named after the post's title.
		"https://preview.redd.it/a-long-title-v0-k3m9x2abc1yh1.jpeg?width=640&s=1": "https://i.redd.it/k3m9x2abc1yh1.jpeg",
	} {
		id, ext := ImageFromURL(in)
		if got := OriginalImageURL(id, ext); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"https://i.redd.it/abc.jpg":   "https://i.redd.it/abc.jpeg",
		"https://i.redd.it/abc.jpeg":  "https://i.redd.it/abc.jpg",
		"https://i.redd.it/abc.png":   "",
		"https://i.imgur.com/abc.jpg": "",
	} {
		if got := OtherJPEGSpelling(in); got != want {
			t.Errorf("other spelling of %s: %q, want %q", in, got, want)
		}
	}
}

// An i.redd.it file without an extension is an image; its type comes from
// the download.
func TestImageWithoutExtension(t *testing.T) {
	p := Post{ID: "p1"}
	p.Classify("https://i.redd.it/9f0c2e7a-55d1-4c3b-8e2f-1a2b3c4d5e6f", "")
	if p.Kind != KindImage || p.Media[0].URL != "https://i.redd.it/9f0c2e7a-55d1-4c3b-8e2f-1a2b3c4d5e6f" ||
		p.MediaKey() != "redditmedia 9f0c2e7a-55d1-4c3b-8e2f-1a2b3c4d5e6f" {
		t.Fatalf("%+v %s", p, p.MediaKey())
	}
}
