package proc_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/proc"
)

func TestMain(m *testing.M) {
	switch os.Getenv("PROC_TEST_MODE") {
	case "child":
		helperChild()
		return
	case "grandchild":
		helperGrandchild()
		return
	case "sleep":
		time.Sleep(10 * time.Minute)
		return
	case "exit3":
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func helperEnv(mode, portFile string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PROC_TEST_MODE=") || strings.HasPrefix(entry, "PROC_TEST_PORT_FILE=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "PROC_TEST_MODE="+mode)
	if portFile != "" {
		env = append(env, "PROC_TEST_PORT_FILE="+portFile)
	}
	return env
}

func helperChild() {
	portFile := os.Getenv("PROC_TEST_PORT_FILE")
	// Give the parent time to assign this process to its Job Object before a
	// grandchild exists, which removes the Windows assignment race.
	time.Sleep(300 * time.Millisecond)

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = helperEnv("grandchild", portFile)
	if err := cmd.Start(); err != nil {
		_ = os.WriteFile(portFile+".error", []byte(err.Error()), 0o600)
		os.Exit(1)
	}
	time.Sleep(10 * time.Minute)
}

func helperGrandchild() {
	portFile := os.Getenv("PROC_TEST_PORT_FILE")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.WriteFile(portFile+".error", []byte(err.Error()), 0o600)
		os.Exit(1)
	}
	if err := os.WriteFile(portFile, []byte(listener.Addr().String()), 0o600); err != nil {
		os.Exit(1)
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		_ = connection.Close()
	}
}

func waitForAddress(t *testing.T, portFile string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(portFile + ".error"); err == nil {
			t.Fatalf("helper failed: %s", data)
		}
		if data, err := os.ReadFile(portFile); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("helper did not report a listening address")
	return ""
}

func dialWorks(address string) bool {
	connection, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func TestNormalExitIsNotCancelled(t *testing.T) {
	t.Parallel()

	handle, err := proc.Start(context.Background(), proc.Options{
		Path: os.Args[0],
		Args: []string{"-test.run=^$"},
		Env:  helperEnv("exit3", ""),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := handle.Wait()
	if result.Cancelled || result.TimedOut {
		t.Fatalf("exited child reported cancelled=%v timedOut=%v", result.Cancelled, result.TimedOut)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", result.ExitCode)
	}
	if result.EndedAt.Before(result.StartedAt) {
		t.Fatal("ended before started")
	}
}

func TestCancellationKillsWholeTree(t *testing.T) {
	if testing.Short() {
		t.Skip("process containment test is slow and skipped in -short mode")
	}

	dir := t.TempDir()
	portFile := filepath.Join(dir, "port")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var output strings.Builder
	handle, err := proc.Start(ctx, proc.Options{
		Path:        os.Args[0],
		Args:        []string{"-test.run=^$"},
		Env:         helperEnv("child", portFile),
		GracePeriod: 300 * time.Millisecond,
		Stdout:      &output,
		Stderr:      &output,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	address := waitForAddress(t, portFile)
	if !dialWorks(address) {
		t.Fatalf("grandchild at %s is not reachable before cancellation", address)
	}

	cancel()
	result := handle.Wait()
	if !result.Cancelled {
		t.Fatalf("result = %+v, want Cancelled", result)
	}
	if result.TimedOut {
		t.Fatalf("result = %+v, want not TimedOut", result)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !dialWorks(address) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild at %s survived cancellation; helper output: %s", address, output.String())
}

func TestDeadlineIsTimedOutNotCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("process containment test is slow and skipped in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	handle, err := proc.Start(ctx, proc.Options{
		Path:        os.Args[0],
		Args:        []string{"-test.run=^$"},
		Env:         helperEnv("sleep", ""),
		GracePeriod: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := handle.Wait()
	if !result.TimedOut {
		t.Fatalf("result = %+v, want TimedOut", result)
	}
	if result.Cancelled {
		t.Fatalf("result = %+v, want not Cancelled", result)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx error = %v", ctx.Err())
	}
}

func TestStartRejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	if _, err := proc.Start(context.Background(), proc.Options{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestHandlePid(t *testing.T) {
	t.Parallel()
	handle, err := proc.Start(context.Background(), proc.Options{
		Path: os.Args[0],
		Args: []string{"-test.run=^$"},
		Env:  helperEnv("exit3", ""),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if handle.Pid() <= 0 {
		t.Fatalf("pid = %d", handle.Pid())
	}
	_ = handle.Wait()
}
