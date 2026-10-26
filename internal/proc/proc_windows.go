//go:build windows

package proc

import (
	"context"
	"os/exec"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsContainment struct {
	mu       sync.Mutex
	job      windows.Handle
	pid      int
	assigned bool
	grace    time.Duration
}

func newContainment(cmd *exec.Cmd, grace time.Duration) containment {
	_ = cmd
	return &windowsContainment{grace: grace}
}

// Attach creates a kill-on-close Job Object and assigns the child to it. If
// any step fails, the unit degrades to a taskkill /T fallback rather than
// failing the invocation.
func (c *windowsContainment) Attach(pid int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pid = pid

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		// #nosec G103 -- Job Object limits require the documented Windows struct layout.
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}

	if pid <= 0 {
		return nil
	}
	// #nosec G115 -- Windows process ids are positive and fit in uint32.
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	defer func() { _ = windows.CloseHandle(process) }()

	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return nil
	}
	c.job = job
	c.assigned = true
	return nil
}

func (c *windowsContainment) Terminate(done <-chan struct{}) {
	c.mu.Lock()
	job := c.job
	assigned := c.assigned
	pid := c.pid
	if assigned && job != 0 {
		_ = windows.TerminateJobObject(job, 1)
	}
	c.mu.Unlock()

	if !assigned && pid > 0 {
		taskkill(pid)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func (c *windowsContainment) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
}

func taskkill(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G204 -- taskkill is the documented Windows containment fallback.
	_ = exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
