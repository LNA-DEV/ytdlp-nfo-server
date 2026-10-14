package ytdlp

import (
	"context"
	"strings"
	"time"
)

// Target is an impersonation target from --list-impersonate-targets.
type Target struct {
	Client    string `json:"client"`
	OS        string `json:"os"`
	Source    string `json:"source"`
	Available bool   `json:"available"`
}

// Name returns the --impersonate value, e.g. "chrome-131:android-14".
func (t Target) Name() string {
	n := strings.ToLower(t.Client)
	if t.OS != "" {
		n += ":" + strings.ToLower(t.OS)
	}
	return n
}

// ParseTargets parses the table printed by --list-impersonate-targets.
// Unavailable targets carry "(unavailable)" in the source column.
func ParseTargets(out string) []Target {
	var targets []Target
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r ")
		if strings.HasPrefix(line, "---") {
			inTable = true
			continue
		}
		if !inTable || strings.TrimSpace(line) == "" || strings.HasPrefix(line, "[") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		t := Target{Client: f[0], Available: true}
		if len(f) >= 2 {
			t.OS = f[1]
		}
		if len(f) >= 3 {
			t.Source = strings.Join(f[2:], " ")
		}
		if strings.Contains(strings.ToLower(line), "unavailable") {
			t.Available = false
		}
		targets = append(targets, t)
	}
	return targets
}

// ListTargets runs --list-impersonate-targets.
func ListTargets(ctx context.Context, bin string) ([]Target, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out strings.Builder
	res := Run(ctx, Call{Bin: bin, Args: ListTargetsArgs(), CaptureStdout: true, Grace: 2 * time.Second,
		OnLine: func(l string) { out.WriteString(l + "\n") }})
	text := string(res.Stdout) + "\n" + out.String()
	if !res.OK() {
		return nil, text, &CallError{Res: res}
	}
	return ParseTargets(text), text, nil
}

// CallError wraps a failed result.
type CallError struct{ Res *Result }

func (e *CallError) Error() string { return e.Res.Describe() }
