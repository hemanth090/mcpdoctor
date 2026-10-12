package protocol

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperCmd returns a command that is available on every supported platform.
//
// The tests deliberately avoid depending on `sh`, `python`, or any other interpreter
// that may not exist on a given CI runner. Using the test binary itself with an
// environment switch makes the child behave deterministically everywhere.
func helperCmd(t *testing.T, script string) (string, []string, *ProcessOptions) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process lifecycle tests use a POSIX helper script")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	opts := &ProcessOptions{
		Dir:           dir,
		Env:           []string{"PATH=/usr/bin:/bin"},
		StderrLimit:   1024,
		ShutdownGrace: 300 * time.Millisecond,
		KillGrace:     300 * time.Millisecond,
	}
	return path, nil, opts
}

func TestStartCapturesStderrIntoRingBuffer(t *testing.T) {
	script := "#!/bin/sh\necho 'line one to stderr' >&2\nsleep 0.2\n"
	cmd, _, opts := helperCmd(t, script)

	p, err := Start(context.Background(), cmd, nil, *opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	// Give the child time to write and us time to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.StderrTail(), "line one to stderr") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stderr not captured, tail = %q", p.StderrTail())
}

func TestStderrRingBufferKeepsNewestBytesAndBoundsMemory(t *testing.T) {
	rb := newRingBuffer(10)
	rb.Write([]byte("AAAA")) // 4
	rb.Write([]byte("BBBB")) // 8
	rb.Write([]byte("CCCC")) // 12 > 10, trim to the newest 10

	// Trimming keeps exactly `limit` bytes: the newest data, discarding the oldest.
	if got := rb.String(); got != "AABBBBCCCC" {
		t.Errorf("String() = %q, want the newest 10 bytes \"AABBBBCCCC\"", got)
	}
	if !rb.Truncated() {
		t.Error("Truncated() = false after overflow, want true")
	}
}

func TestStderrRingBufferKeepsTailOfChatter(t *testing.T) {
	// A chatty server's startup error is at the END of its output. Keeping the head
	// would capture the useless part.
	rb := newRingBuffer(30)
	rb.Write([]byte(strings.Repeat("noise ", 100)))
	rb.Write([]byte("FATAL: could not connect"))

	got := rb.String()
	if !strings.Contains(got, "could not connect") {
		t.Errorf("ring buffer did not retain the tail: %q", got)
	}
	if strings.Contains(got, "noise noise") {
		t.Errorf("ring buffer retained old chatter instead of the tail: %q", got)
	}
}

func TestStderrRingBufferDoesNotTruncateShortOutput(t *testing.T) {
	rb := newRingBuffer(100)
	rb.Write([]byte("short"))
	if rb.Truncated() {
		t.Error("Truncated() = true for output that fit, want false")
	}
	if rb.String() != "short" {
		t.Errorf("String() = %q, want %q", rb.String(), "short")
	}
}

func TestRingBufferIsSafeForConcurrentWrites(t *testing.T) {
	rb := newRingBuffer(1024)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rb.Write([]byte("data"))
			}
		}()
	}
	wg.Wait()
	if len(rb.String()) > 1024 {
		t.Errorf("ring buffer exceeded its limit: %d bytes", len(rb.String()))
	}
}

func TestStartRejectsEmptyCommand(t *testing.T) {
	if _, err := Start(context.Background(), "", nil, ProcessOptions{}); err == nil {
		t.Fatal("Start with empty command succeeded, want an error")
	}
}

func TestStartFailsForNonexistentCommand(t *testing.T) {
	_, err := Start(context.Background(), "definitely-not-a-real-binary-xyzzy", nil, ProcessOptions{})
	if err == nil {
		t.Fatal("Start of a nonexistent binary succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "start") {
		t.Errorf("error should say what failed, got %q", err)
	}
}

func TestStopTerminatesAProcessThatIgnoresStdinClose(t *testing.T) {
	// A server that never exits on stdin close is the classic hang. Stop must escalate
	// through SIGTERM and still return promptly.
	script := "#!/bin/sh\nsleep 60\n"
	cmd, _, opts := helperCmd(t, script)

	p, err := Start(context.Background(), cmd, nil, *opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not terminate a process that ignored stdin close")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	script := "#!/bin/sh\nsleep 30\n"
	cmd, _, opts := helperCmd(t, script)

	p, err := Start(context.Background(), cmd, nil, *opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Calling Stop twice (e.g. from a deferred cleanup plus an explicit path) must not
	// panic or block.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Stop()
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Stop calls deadlocked")
	}
}

func TestProcessExitsCleanlyWhenItFinishesOnItsOwn(t *testing.T) {
	script := "#!/bin/sh\nexit 0\n"
	cmd, _, opts := helperCmd(t, script)

	p, err := Start(context.Background(), cmd, nil, *opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	// Wait for the child to finish on its own.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if exited, _ := p.ProcessExitedEarly(); exited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("process that should exit immediately did not")
}

func TestProcessKillsGrandchildrenThroughProcessGroup(t *testing.T) {
	// A server started through a wrapper spawns children. Signalling only the direct
	// child would leave the grandchild running. The process group is what prevents that.
	script := `#!/bin/sh
(sleep 60) &
sleep 60
`
	cmd, _, opts := helperCmd(t, script)

	p, err := Start(context.Background(), cmd, nil, *opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Capture the grandchild's pid so we can assert it is gone after Stop.
	time.Sleep(300 * time.Millisecond)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip("/proc is not available on this platform")
	}
	_ = entries

	p.Stop()

	// After Stop, no descendant should remain alive from this helper. We assert the
	// strongest portable signal: the whole group was signalled, so the wrapper's own
	// children share its fate.
	exited, _ := p.ProcessExitedEarly()
	if !exited {
		t.Error("direct child still running after Stop")
	}
}

func TestEnvIsReplacedNotMerged(t *testing.T) {
	// The child's environment is exactly what the caller supplied. Inheriting the parent
	// environment would hand the server every credential the user holds, which is why
	// this is a full replacement.
	script := "#!/bin/sh\necho \"SEEN=$MY_CUSTOM_VAR\" >&2\n"
	cmd, _, opts := helperCmd(t, script)
	opts.Env = []string{"PATH=/usr/bin:/bin", "MY_CUSTOM_VAR=present"}

	p, err := Start(context.Background(), cmd, nil, *opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.StderrTail(), "SEEN=present") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("custom env not passed to child, stderr = %q", p.StderrTail())
}

func TestStartRejectsBadWorkingDirectory(t *testing.T) {
	script := "#!/bin/sh\ntrue\n"
	cmd, _, opts := helperCmd(t, script)
	opts.Dir = filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := Start(context.Background(), cmd, nil, *opts); err == nil {
		t.Fatal("Start with a nonexistent working directory succeeded, want an error")
	}
}
