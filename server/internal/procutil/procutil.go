// Package procutil runs child processes in their own process group so that
// cancellation reaches every descendant (yt-dlp spawns ffmpeg), and stops them
// gracefully: SIGINT to the group first, SIGKILL after a grace period.
package procutil

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultGrace is how long a process group gets to exit after SIGINT.
const DefaultGrace = 15 * time.Second

// ErrInterrupted is returned (wrapped) when the process was stopped because
// the context was cancelled.
var ErrInterrupted = errors.New("process interrupted")

// Run starts cmd in a new process group and waits for it. When ctx is
// cancelled the whole group receives SIGINT, then SIGKILL after grace.
// Run only returns after the process has been reaped, so callers may safely
// delete the process's working directory afterwards.
func Run(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	if err := Start(cmd); err != nil {
		return err
	}
	return Wait(ctx, cmd, grace)
}

// Start starts cmd in its own process group.
func Start(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	// If the server dies (crash, kill -9) the child is killed too, so no
	// orphaned yt-dlp keeps writing into a work dir that the restarted
	// server resumes.
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if cmd.WaitDelay == 0 {
		// Bounds how long Wait blocks on stdout/stderr held open by orphaned
		// grandchildren after the leader exited.
		cmd.WaitDelay = 5 * time.Second
	}
	return cmd.Start()
}

// Wait waits for a command started with Start, handling cancellation.
func Wait(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	if grace <= 0 {
		grace = DefaultGrace
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	var interrupted atomic.Bool
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		interrupted.Store(true)
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()
	err := cmd.Wait()
	close(done)
	// Reap anything left in the group (e.g. an ffmpeg child that outlived
	// yt-dlp). The pgid cannot be reused while members still exist.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if interrupted.Load() {
		return errors.Join(ErrInterrupted, context.Cause(ctx))
	}
	return err
}

// ExitCode returns the exit code from an exec error, -1 if the process was
// killed by a signal, and 0 for a nil error.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return -1
		}
		return ee.ExitCode()
	}
	return -2
}

// Signaled reports whether the process was terminated by a signal and which.
func Signaled(err error) (syscall.Signal, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal(), true
		}
	}
	return 0, false
}
