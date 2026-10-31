package subscriptions

import (
	"strings"
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

func TestFilters(t *testing.T) {
	f, err := CompileFilters(store.Filters{MinDuration: 120, MaxDuration: 3600, TitleExclude: "trailer|teaser", PublishedAfter: "20240101"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		e    store.Entry
		want string
	}{
		{store.Entry{Title: "Ok", Duration: 600, UploadDate: "20240505"}, ""},
		{store.Entry{Title: "Short", Duration: 59}, "filter: shorter than 2:00"},
		{store.Entry{Title: "Long", Duration: 7200}, "filter: longer than 1:00:00"},
		{store.Entry{Title: "Official TRAILER"}, `filter: title matches "trailer|teaser"`},
		{store.Entry{Title: "Old", UploadDate: "20231231"}, "filter: published before 2024-01-01"},
		{store.Entry{Title: "Unknown duration and date"}, ""}, // unknown values pass (§14 decision 19)
	}
	for _, c := range cases {
		if got := f.Reason(&c.e); got != c.want {
			t.Errorf("%s: %q, want %q", c.e.Title, got, c.want)
		}
	}
	inc, _ := CompileFilters(store.Filters{TitleInclude: "^part"})
	if inc.Reason(&store.Entry{Title: "Part 3"}) != "" || inc.Reason(&store.Entry{Title: "Extra"}) == "" {
		t.Error("include pattern")
	}
	// A listing without titles: title patterns can't tell, so it passes.
	anyTitle, _ := CompileFilters(store.Filters{TitleInclude: "^part", TitleExclude: ".*"})
	if got := anyTitle.Reason(&store.Entry{Duration: 600}); got != "" {
		t.Errorf("unknown title: %q", got)
	}
	long := strings.Repeat("ä", 50)
	if got, _ := CompileFilters(store.Filters{TitleExclude: long}); got.Reason(&store.Entry{Title: long}) !=
		`filter: title matches "`+strings.Repeat("ä", 40)+`…"` {
		t.Errorf("a long pattern must be shortened by characters: %q", got.Reason(&store.Entry{Title: long}))
	}
	none, _ := CompileFilters(store.Filters{})
	if none.Active() || none.Reason(&store.Entry{Duration: 1}) != "" {
		t.Error("empty filters must pass everything")
	}
	for _, bad := range []store.Filters{{TitleExclude: "("}, {TitleInclude: strings.Repeat("a", 201)},
		{MinDuration: 10, MaxDuration: 5}, {PublishedAfter: "2024-01-01"}} {
		if _, err := CompileFilters(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if !IsFilterReason("filter: x") || IsFilterReason(ReasonNeedsCookie) {
		t.Error("IsFilterReason")
	}
}
