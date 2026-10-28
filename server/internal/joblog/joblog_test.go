package joblog

import (
	"strings"
	"testing"
)

func TestCapKeepsHeadAndTail(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.MaxSize = 256 << 10
	l := m.Get(1)
	l.Line("=== header ===")
	for i := 0; i < 10000; i++ {
		l.Printf("line %06d %s", i, strings.Repeat("x", 40))
	}
	l.Line("last line")
	b, _ := m.Read(1)
	if len(b) > 256<<10+4096 {
		t.Fatalf("log not capped: %d bytes", len(b))
	}
	s := string(b)
	if !strings.Contains(s, "=== header ===") || !strings.Contains(s, "last line") || !strings.Contains(s, "truncated") {
		t.Fatal("head, tail or marker missing")
	}
	tail := m.Tail(1, 2)
	if len(tail) != 2 || !strings.HasSuffix(tail[1], "last line") {
		t.Fatalf("tail = %v", tail)
	}
}

func TestSubscribeAndDelete(t *testing.T) {
	m, _ := New(t.TempDir())
	l := m.Get(7)
	l.Line("before")
	snap, ch, cancel := m.Subscribe(7)
	defer cancel()
	if !strings.Contains(string(snap), "before") {
		t.Fatal("snapshot missing")
	}
	l.Line("after")
	if got := <-ch; !strings.HasSuffix(got, "after") {
		t.Fatalf("got %q", got)
	}
	m.Delete(7)
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed on delete")
	}
	if b, _ := m.Read(7); len(b) != 0 {
		t.Fatal("log not deleted")
	}
}

func TestTailOnly(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.MaxSize, m.TailOnly = 4096, true
	l := m.Get(1)
	for i := 0; i < 500; i++ {
		l.Printf("line %d", i)
	}
	b, _ := m.Read(1)
	if len(b) > 4096 || strings.Contains(string(b), "line 0\n") || !strings.Contains(string(b), "line 499") {
		t.Fatalf("tail-only log: %d bytes\n%s", len(b), b)
	}
}
