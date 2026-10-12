//go:build !windows

package protocol

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup places the child in its own process group.
//
// This is the Unix implementation. Creating a new group detaches the child from ours, so
// a Ctrl-C aimed at our group does not reach it implicitly, and any signal we send
// deliberately reaches the entire tree.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalGroup sends sig to the child's process group.
//
// Because Setpgid made the child its own group leader, its process group id equals its
// pid. Signalling the group rather than the single pid is what catches grandchildren,
// which an MCP server started through a wrapper script will usually have created.
func signalGroup(proc *os.Process, sig syscall.Signal) {
	if proc == nil {
		return
	}
	_ = syscall.Kill(-proc.Pid, sig)
}

// terminateSignal is the graceful termination signal on this platform.
const terminateSignal = syscall.SIGTERM

// killSignal is the forced termination signal on this platform.
const killSignal = syscall.SIGKILL

// signalSupported reports whether this platform has POSIX signals.
const signalSupported = true
