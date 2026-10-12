//go:build windows

package protocol

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup places the child in a new process group on Windows.
//
// CREATE_NEW_PROCESS_GROUP is the Windows analogue of Setpgid. It matters for the same
// reason: a Ctrl-C in the shared console is delivered to every process attached to it,
// so a child in its own group is signalled only when we mean it to be.
//
// The documented limitation is that Windows has no equivalent of kill(-pgid), so a
// process that spawns children is not signalled as a group. See the security model:
// MCP Doctor's process containment is weaker on Windows than on Unix, and the README
// says so rather than implying a guarantee it cannot keep.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// signalGroup terminates the process on Windows.
//
// There is no signal equivalent to kill(-pgid), so this falls back to terminating the
// direct child only.
func signalGroup(proc *os.Process, sig syscall.Signal) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
}

// terminateSignal and killSignal are unused on Windows, where termination is
// unconditional. They are defined so that cross-platform code compiles unchanged.
const (
	terminateSignal = syscall.Signal(0)
	killSignal      = syscall.Signal(0)
)

// signalSupported reports whether POSIX-style graceful signalling is available.
const signalSupported = false
