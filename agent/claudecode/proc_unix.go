//go:build unix

package claudecode

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// prepareCmdForKill puts the spawned child into its own process group so that
// the entire descendant tree can be terminated with a single signal aimed at
// the negative PID. Without this, cc-connect can only signal the direct
// child (e.g. the `claude` CLI), leaving any grandchildren (MCP server
// processes such as the Telegram bridge) as orphans that may spin at 100%
// CPU when their parent disappears.
//
// Mirrors the pattern used by agent/codex/proc_unix.go.
func prepareCmdForKill(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// installGroupCancel makes context cancellation terminate the whole process
// group. exec.CommandContext's default Cancel only kills the direct child, so
// a cancellation (e.g. Engine.Stop cancelling e.ctx before Close runs)
// orphaned every MCP server. cmd.WaitDelay then escalates to SIGKILL for the
// child, and sweepProcessGroup finishes off any grandchild still standing.
// Only valid on a cmd created with exec.CommandContext.
func installGroupCancel(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.Cancel = func() error {
		return signalProcessGroup(cmd, syscall.SIGTERM)
	}
}

// sweepProcessGroup SIGKILLs whatever is left of cmd's process group once the
// direct child has been reaped. Called right after cmd.Wait() returns on every
// exit path (clean exit, idle close, eviction, shutdown), so MCP servers and
// anything they spawned never outlive the session.
func sweepProcessGroup(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGKILL)
}

// signalProcessGroup sends sig to the entire process group rooted at cmd.
// Returns nil if the group is already gone.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil &&
		!errors.Is(err, os.ErrProcessDone) &&
		!errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// forceKillCmd SIGKILLs the entire process group rooted at cmd. Use this
// as the last-resort escalation when graceful shutdown has timed out.
func forceKillCmd(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGKILL)
}
