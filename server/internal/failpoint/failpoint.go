// Package failpoint provides named crash points for integrity tests.
//
// In production no failpoint is active and Check is a map lookup. Setting
// YTDLP_NFO_FAILPOINTS=name1,name2 makes the process SIGKILL itself when it
// reaches one of those points, which simulates a power loss or kill -9.
// Unit tests can instead install a Hook that turns a point into an error.
package failpoint

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
)

var (
	mu     sync.RWMutex
	active = parse(os.Getenv("YTDLP_NFO_FAILPOINTS"))
	hook   func(name string) error
)

func parse(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			m[p] = true
		}
	}
	return m
}

// Check is called at a named point. It kills the process if the point is
// active via the environment, or returns the hook's error in tests.
func Check(name string) error {
	mu.RLock()
	h := hook
	kill := active[name]
	mu.RUnlock()
	if kill {
		log.Printf("failpoint %s reached: killing process", name)
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // never returns
	}
	if h != nil {
		if err := h(name); err != nil {
			return fmt.Errorf("failpoint %s: %w", name, err)
		}
	}
	return nil
}

// SetHook installs a test hook and returns a function restoring the old one.
func SetHook(h func(name string) error) func() {
	mu.Lock()
	old := hook
	hook = h
	mu.Unlock()
	return func() {
		mu.Lock()
		hook = old
		mu.Unlock()
	}
}
