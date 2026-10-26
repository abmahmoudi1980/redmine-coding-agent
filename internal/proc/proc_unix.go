//go:build unix

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

type unixContainment struct {
	pid   int
	grace time.Duration
}

func newContainment(cmd *exec.Cmd, grace time.Duration) containment {
	// A dedicated process group makes kill(-pgid) reach every descendant that
	// did not deliberately leave the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &unixContainment{grace: grace}
}

func (c *unixContainment) Attach(pid int) error {
	c.pid = pid
	return nil
}

func (c *unixContainment) Terminate(done <-chan struct{}) {
	if c.pid <= 0 {
		return
	}
	// Polite first, so a cooperative agent can flush its transcript.
	_ = syscall.Kill(-c.pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(c.grace):
	}
	// Always force the group: grandchildren may have ignored SIGTERM.
	_ = syscall.Kill(-c.pid, syscall.SIGKILL)
}

func (c *unixContainment) Release() {}
