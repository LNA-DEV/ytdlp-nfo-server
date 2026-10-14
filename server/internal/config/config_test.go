package config

import "testing"

func TestNormalizeURL(t *testing.T) {
	same := [][]string{
		{"https://youtu.be/abc123", "https://www.youtube.com/watch?v=abc123", "http://m.youtube.com/watch?v=abc123&feature=share", "https://youtube.com/shorts/abc123?si=x"},
		{"https://example.com/video/1#t=10", "https://www.example.com/video/1/", "https://example.com/video/1?utm_source=x"},
		{"https://example.com/v?b=2&a=1", "https://example.com/v?a=1&b=2"},
	}
	for _, group := range same {
		want := NormalizeURL(group[0])
		for _, u := range group[1:] {
			if got := NormalizeURL(u); got != want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", u, got, want)
			}
		}
	}
	if NormalizeURL("https://example.com/a") == NormalizeURL("https://example.com/b") {
		t.Error("different paths compare equal")
	}
	if NormalizeURL("https://www.youtube.com/watch?v=a&list=PL1") == NormalizeURL("https://www.youtube.com/watch?v=a") {
		t.Error("playlist parameter dropped")
	}
}

func TestSiteKey(t *testing.T) {
	cases := map[string]string{
		"www.youtube.com":   "youtube.com",
		"youtu.be":          "youtube.com",
		"cdn.a.example.com": "example.com",
		"news.bbc.co.uk":    "bbc.co.uk",
		"localhost":         "localhost",
	}
	for in, want := range cases {
		if got := SiteKey(in); got != want {
			t.Errorf("SiteKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	bad := []string{"", "-o /etc/passwd", "--exec=rm", "file:///etc/passwd", "ftp://x", "https://", "https://a b", "https://x\n"}
	for _, u := range bad {
		if ValidateURL(u) == nil {
			t.Errorf("ValidateURL(%q) accepted", u)
		}
	}
	if err := ValidateURL("https://example.com/watch?v=1"); err != nil {
		t.Error(err)
	}
}

func TestSiteRuleMatch(t *testing.T) {
	r := SiteRule{Pattern: "*.example.com"}
	for host, want := range map[string]bool{"example.com": true, "a.example.com": true, "notexample.com": false} {
		if r.Matches(host) != want {
			t.Errorf("%q: got %v", host, !want)
		}
	}
	r2 := SiteRule{Pattern: "example.org"}
	if !r2.Matches("www.example.org") || r2.Matches("badexample.org") {
		t.Error("plain pattern matching wrong")
	}
}

func TestResolvePrecedence(t *testing.T) {
	s := DefaultSettings(false)
	rules := []SiteRule{{ID: 7, Pattern: "*.example.com", Impersonate: "chrome", Preset: "max720", CookieProfile: "acct"}}
	// site rule beats default
	o, err := Resolve(OptionsRequest{}, &s, rules, "https://www.example.com/v")
	if err != nil {
		t.Fatal(err)
	}
	if o.Impersonate != "chrome" || o.Preset != "max720" || o.CookieProfile != "acct" || o.SiteRuleID != 7 {
		t.Fatalf("rule not applied: %+v", o)
	}
	// explicit option beats site rule, legacy "format" maps to container
	off, mp4 := "", "mp4"
	o, err = Resolve(OptionsRequest{Impersonate: &off, Format: &mp4}, &s, rules, "https://www.example.com/v")
	if err != nil {
		t.Fatal(err)
	}
	if o.Impersonate != "" || o.Container != "mp4" {
		t.Fatalf("explicit not applied: %+v", o)
	}
	// unrelated host keeps defaults
	o, _ = Resolve(OptionsRequest{}, &s, rules, "https://other.org/v")
	if o.Impersonate != "" || o.Preset != "best" || !o.AllAudio || !o.Subtitles || o.Container != "mkv" {
		t.Fatalf("defaults wrong: %+v", o)
	}
	bad := "nope"
	if _, err := Resolve(OptionsRequest{Preset: &bad}, &s, nil, "https://x.org"); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestSettingsValidate(t *testing.T) {
	s := DefaultSettings(false)
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.Presets["evil"] = "--exec rm"
	if s.Validate() == nil {
		t.Fatal("option injection via preset accepted")
	}
}

func TestVersionSpec(t *testing.T) {
	for _, ok := range []string{"", "nightly:latest", "stable/2026.08.19", "nightly/2026.10.10.233141"} {
		if err := ValidateVersionSpec(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"beta:latest", "stable/../x", "stable/", "x/y"} {
		if ValidateVersionSpec(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
