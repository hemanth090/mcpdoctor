package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ProcessOptions configures how a diagnostic subprocess is launched.
type ProcessOptions struct {
	// Dir is the working directory for the child. Empty means inherit the current one.
	Dir string

	// Env is the complete environment for the child.
	//
	// This is a full replacement, never a merge. The caller builds it from an explicit
	// allowlist rather than calling os.Environ(), so that the tool never hands the
	// server credentials the operator did not intend to share. An MCP server that
	// inherits everything also inherits things like cloud tokens.
	Env []string

	// StderrLimit is the maximum number of bytes of stderr retained, held in a ring
	// buffer. Server output is untrusted and unbounded, so keeping only the tail bounds
	// memory while preserving the part that matters, which is the end.
	StderrLimit int

	// ShutdownGrace is how long to wait after closing stdin before sending SIGTERM.
	// Zero uses DefaultShutdownGrace.
	ShutdownGrace time.Duration

	// KillGrace is how long to wait after SIGTERM before sending SIGKILL. Zero uses
	// DefaultKillGrace.
	KillGrace time.Duration
}

// Default timeouts for the spec's shutdown ladder.
const (
	// DefaultShutdownGrace is the wait after closing stdin, before SIGTERM. The MCP
	// specification prescribes exactly this sequence for stdio shutdown.
	DefaultShutdownGrace = 5 * time.Second

	// DefaultKillGrace is the wait after SIGTERM, before SIGKILL.
	DefaultKillGrace = 3 * time.Second

	// DefaultStderrLimit bounds retained stderr. 64 KiB is far more than any readable
	// diagnostic excerpt needs and small enough to be irrelevant.
	DefaultStderrLimit = 64 << 10
)

// ErrNotStarted is returned when a Process method is called before Start succeeds.
var ErrNotStarted = errors.New("process was not started")

// Process is a diagnostic subprocess with a managed lifecycle.
//
// It owns exactly one responsibility: starting a process, keeping a bounded copy of what
// it writes to stderr, and guaranteeing the process is gone when Stop returns. It does
// not touch stdout, which belongs to the MCP protocol stream; writing anything of ours
// there would corrupt the diagnosis we are attempting.
type Process struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser // the child's stdout pipe, passed to the protocol transport
	stdin  io.WriteCloser
	stderr *ringBuffer

	shutdownGrace time.Duration
	killGrace     time.Duration

	// waitOnce guards cmd.Wait so it runs exactly once; the SDK transport and our own
	// Stop may both want to reap the child.
	waitOnce sync.Once
	waitErr  error

	// done is closed once Wait has reaped the child. Completion is signalled through this
	// channel rather than by polling cmd.ProcessState, because ProcessState is written
	// by Wait and read here, and that read would be a data race.
	done chan struct{}

	stopOnce sync.Once
}

// Start launches the command and begins capturing stderr.
//
// The child is placed in its own process group on Unix so that signals reach the whole
// tree. An MCP server started through a wrapper script routinely spawns children of its
// own; signalling only the direct child would leave those running.
func Start(ctx context.Context, command string, args []string, opts ProcessOptions) (*Process, error) {
	if command == "" {
		return nil, errors.New("command is empty")
	}

	// Never invoke a shell. exec.Command with an argv slice passes arguments directly,
	// so a value like "foo; rm -rf /" is one literal argument, not a shell command.
	cmd := exec.Command(command, args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	grace := opts.ShutdownGrace
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}
	kill := opts.KillGrace
	if kill <= 0 {
		kill = DefaultKillGrace
	}

	limit := opts.StderrLimit
	if limit <= 0 {
		limit = DefaultStderrLimit
	}

	rb := newRingBuffer(limit)
	// Route the child's stderr into the ring buffer. We never forward it to our own
	// stderr, because that would interleave with our diagnostics and confuse the user.
	// Route the child's stderr into the ring buffer. We never forward it to our own
	// stderr, because that would interleave with our diagnostics and confuse the user.
	stderrPipe, perr := cmd.StderrPipe()
	if perr != nil {
		return nil, fmt.Errorf("stderr pipe: %w", perr)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	p := &Process{
		cmd:           cmd,
		stdout:        stdout,
		stdin:         stdin,
		stderr:        rb,
		shutdownGrace: grace,
		killGrace:     kill,
		done:          make(chan struct{}),
	}
	go p.drainStderr(stderrPipe)
	return p, nil
}

// Stdout returns the child's stdout pipe for the MCP transport to consume.
func (p *Process) Stdout() io.ReadCloser { return p.stdout }

// Stdin returns the write end of the child's stdin pipe for the MCP transport to use.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// StderrTail returns the most recent stderr bytes, bounded by the configured limit.
//
// It is captured after the process exits, because that is when the final error message
// has been written.
func (p *Process) StderrTail() string { return p.stderr.String() }

// drainStderr reads the child's stderr into the ring buffer until EOF.
func (p *Process) drainStderr(pipe io.ReadCloser) {
	buf := make([]byte, 4096)
	for {
		n, err := pipe.Read(buf)
		if n > 0 {
			p.stderr.Write(buf[:n])
		}
		if err != nil {
			_ = pipe.Close()
			return
		}
	}
}

// Wait reaps the child and returns its exit error, if any.
//
// The child is always reaped, even on the error paths, so that Stop never leaves a
// zombie behind. Wait is safe to call from multiple goroutines and always returns the
// same result.
func (p *Process) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	})
	return p.waitErr
}

// Done returns a channel closed once the child has been reaped.
//
// Completion is signalled through this channel rather than by inspecting the process
// table: cmd.ProcessState is written by Wait, so reading it from another goroutine is a
// data race that -race correctly reports and that would in production be a subtle bug.
func (p *Process) Done() <-chan struct{} { return p.done }

// Stop terminates the process following the MCP specification's shutdown ladder and
// guarantees it is gone before returning.
//
// The sequence is the one the stdio transport section prescribes: close stdin, wait for
// the server to exit, send SIGTERM, then SIGKILL. It is also the correct sequence for any
// subprocess, because it gives a well-behaved process the chance to flush and clean up
// before being forced.
func (p *Process) Stop() {
	p.stopOnce.Do(func() {
		p.stopInternal()
	})
}

// stopInternal performs the shutdown ladder.
func (p *Process) stopInternal() {
	if p.cmd.Process == nil {
		return
	}

	// Step 1: close stdin so the server observes end-of-input, which is the documented
	// graceful signal for the stdio transport.
	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	// Step 2: give it a bounded chance to exit on its own.
	if p.waitWithTimeout(p.shutdownGrace) {
		return
	}

	// Step 3: SIGTERM the whole process group, so wrapper scripts and grandchildren go
	// too.
	signalGroup(p.cmd.Process, syscall.SIGTERM)

	// Step 4: if it ignores SIGTERM, escalate.
	if p.waitWithTimeout(p.killGrace) {
		return
	}
	signalGroup(p.cmd.Process, syscall.SIGKILL)

	// Step 5: reap unconditionally so no zombie remains.
	_ = p.Wait()
}

// waitWithTimeout reports whether the process exited within d.
func (p *Process) waitWithTimeout(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

// setProcessGroup and signalGroup are implemented per platform in process_unix.go and
// process_windows.go, because the mechanisms differ enough that a shared implementation
// would need runtime checks on every call.

// ProcessExitedEarly reports whether the process has already finished, and its exit
// error if so.
//
// It is used to distinguish "the server crashed" from "the server is slow", which are
// completely different diagnoses that otherwise present as the same timeout.
// ProcessExitedEarly reports whether the process has already finished.
//
// It is used to distinguish "the server crashed" from "the server is slow", which are
// completely different diagnoses that otherwise present as the same timeout. Completion
// is observed through the done channel, so there is no data race.
//
// The reaper is started as a side effect: if nobody has called Wait yet, this ensures one
// is running so that the done channel eventually closes.
func (p *Process) ProcessExitedEarly() (bool, error) {
	// Kick off the reaper so done will close if the process exits.
	go func() { _ = p.Wait() }()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.done:
		return true, nil
	case <-timer.C:
		return false, nil
	}
}

// ringBuffer is a fixed-capacity buffer that keeps the most recent bytes written to it.
//
// A plain truncation would drop the newest output, which for a startup failure is
// exactly the part that explains the failure. Keeping the tail is what makes the
// captured stderr useful.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
	full  bool
}

// newRingBuffer returns a ring buffer holding at most limit bytes.
func newRingBuffer(limit int) *ringBuffer {
	if limit <= 0 {
		limit = DefaultStderrLimit
	}
	return &ringBuffer{buf: make([]byte, 0, limit), limit: limit}
}

// Write appends b, discarding the oldest bytes once the limit is exceeded.
func (r *ringBuffer) Write(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buf = append(r.buf, b...)
	if len(r.buf) > r.limit {
		// Keep the newest limit bytes.
		r.buf = r.buf[len(r.buf)-r.limit:]
		r.full = true
	}
}

// String returns the retained bytes as a string.
func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return string(out)
}

// Truncated reports whether any bytes were discarded.
func (r *ringBuffer) Truncated() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.full
}
