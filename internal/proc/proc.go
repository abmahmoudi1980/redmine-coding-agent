// Package proc starts child processes inside an OS containment unit so that
// cancellation kills the whole process tree, not just the direct child.
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultGracePeriod is how long a child gets between the polite and the
// forceful kill on cancellation.
const DefaultGracePeriod = 5 * time.Second

// Options describe one child process.
type Options struct {
	Path        string
	Args        []string
	Dir         string
	Env         []string
	Stdout      io.Writer
	Stderr      io.Writer
	GracePeriod time.Duration
}

// Result is what the parent observed, independent of platform.
type Result struct {
	ExitCode  int
	TimedOut  bool
	Cancelled bool
	StartedAt time.Time
	EndedAt   time.Time
}

// Handle owns one running child and its containment unit.
type Handle struct {
	cmd         *exec.Cmd
	containment containment

	done          chan struct{}
	cancelDone    chan struct{}
	cancelStarted atomic.Bool
	releaseOnce   sync.Once

	mu      sync.Mutex
	result  Result
	waitErr error
}

// Start launches the child and immediately attaches it to its containment
// unit. A context deadline produces TimedOut; explicit cancellation produces
// Cancelled.
func Start(ctx context.Context, opts Options) (*Handle, error) {
	if opts.Path == "" {
		return nil, errors.New("proc: command path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	grace := opts.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}

	// #nosec G204 -- this package exists to start caller-chosen executables.
	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	cmd.Stdin = nil

	unit := newContainment(cmd, grace)
	if err := cmd.Start(); err != nil {
		unit.Release()
		return nil, fmt.Errorf("proc: start %s: %w", opts.Path, err)
	}
	if err := unit.Attach(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		unit.Release()
		return nil, fmt.Errorf("proc: contain %s: %w", opts.Path, err)
	}

	handle := &Handle{
		cmd:         cmd,
		containment: unit,
		done:        make(chan struct{}),
		cancelDone:  make(chan struct{}),
	}
	handle.result.StartedAt = time.Now()
	go handle.reap()
	go handle.watch(ctx)
	return handle, nil
}

// Pid is the direct child's process id.
func (h *Handle) Pid() int {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// Done is closed when the direct child has been reaped.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Wait blocks until the child and any containment cleanup are finished.
func (h *Handle) Wait() Result {
	if h == nil {
		return Result{}
	}
	<-h.done
	if h.cancelStarted.Load() {
		select {
		case <-h.cancelDone:
		case <-time.After(35 * time.Second):
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result
}

// Err is the underlying exec error, if the child exited non-zero.
func (h *Handle) Err() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waitErr
}

func (h *Handle) reap() {
	err := h.cmd.Wait()
	ended := time.Now()

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}

	h.mu.Lock()
	h.waitErr = err
	h.result.ExitCode = code
	h.result.EndedAt = ended
	h.mu.Unlock()

	h.releaseOnce.Do(h.containment.Release)
	close(h.done)
}

func (h *Handle) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		h.mu.Lock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.result.TimedOut = true
		} else {
			h.result.Cancelled = true
		}
		h.mu.Unlock()

		h.cancelStarted.Store(true)
		h.containment.Terminate(h.done)
		h.releaseOnce.Do(h.containment.Release)
		close(h.cancelDone)
	case <-h.done:
	}
}

// containment is the platform-specific process tree boundary.
type containment interface {
	Attach(pid int) error
	Terminate(done <-chan struct{})
	Release()
}
