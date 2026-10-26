// Command fakeagent is a controllable coding agent used by the integration
// harness. It speaks the JSONL + `## RESULT` shape from
// contracts/agent-adapter.md and never touches the network.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	mode := resolveMode(os.Args[1:])
	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeagent: no working directory:", err)
		os.Exit(1)
	}

	switch mode {
	case "sleep":
		time.Sleep(10 * time.Minute)
		return
	case "hang":
		emit(map[string]any{"type": "activity", "message": "hanging on purpose"})
		fmt.Println("waiting for cancellation")
		time.Sleep(10 * time.Minute)
		return
	case "crash":
		fmt.Fprintln(os.Stderr, "fakeagent: crashing without a result block")
		os.Exit(1)
	}

	emit(map[string]any{"type": "assistant", "text": "Reading the issue and the repository."})
	emit(map[string]any{"type": "tool", "name": "read", "path": "README.md"})

	switch mode {
	case "ok":
		writeChange(workspace, "implemented by fakeagent")
		emit(map[string]any{"type": "tool", "name": "edit", "path": "agent-change.txt"})
		writeResult("done", "agent-change.txt", "unit-tests: passed (agent claim)", "(none)")
	case "lies":
		writeChange(workspace, "this change will fail the worker's own check")
		emit(map[string]any{"type": "tool", "name": "edit", "path": "agent-change.txt"})
		writeResult("done", "agent-change.txt", "unit-tests: passed (agent claim)", "(none)")
	case "no-changes":
		emit(map[string]any{"type": "assistant", "text": "No changes were necessary."})
		writeResult("done", "(none)", "(none)", "(none)")
	case "blocked":
		emit(map[string]any{"type": "assistant", "text": "I need an answer before changing code."})
		writeResult("blocked", "(none)", "(none)", "Which database should the migration target?")
	case "escape-path":
		target := filepath.Join(filepath.Dir(workspace), "fakeagent-escape-"+filepath.Base(workspace)+".txt")
		emit(map[string]any{
			"type": "permission_denied", "tool": "write", "path": target,
			"message": "blocked write outside the workspace",
		})
		_ = os.WriteFile(target, []byte("escaped\n"), 0o600)
		writeResult("done", "(none)", "(none)", "(none)")
	case "grandchild":
		// #nosec G204 G702 -- the fake agent re-executes its own binary for the containment test.
		child := exec.Command(os.Args[0], "--mode", "sleep")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "fakeagent: grandchild failed:", err)
			os.Exit(1)
		}
		emit(map[string]any{"type": "activity", "message": fmt.Sprintf("spawned grandchild %d", child.Process.Pid)})
		time.Sleep(10 * time.Minute)
	default:
		writeChange(workspace, "unknown mode fallback")
		writeResult("done", "agent-change.txt", "(none)", "(none)")
	}
}

func resolveMode(args []string) string {
	mode := ""
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--mode" && index+1 < len(args):
			index++
			mode = args[index]
		case strings.HasPrefix(arg, "--mode="):
			mode = strings.TrimPrefix(arg, "--mode=")
		}
		if marker := strings.Index(arg, "FAKE_AGENT_MODE="); marker >= 0 {
			mode = arg[marker+len("FAKE_AGENT_MODE="):]
			if fields := strings.Fields(mode); len(fields) > 0 {
				mode = fields[0]
			}
		}
		if marker := strings.Index(arg, "[fake-agent:"); marker >= 0 {
			rest := arg[marker+len("[fake-agent:"):]
			if end := strings.Index(rest, "]"); end >= 0 {
				mode = rest[:end]
			}
		}
	}
	if mode == "" {
		mode = os.Getenv("FAKE_AGENT_MODE")
	}
	if mode == "" {
		mode = "ok"
	}
	return strings.TrimSpace(mode)
}

func writeChange(workspace, content string) {
	_ = os.WriteFile(filepath.Join(workspace, "agent-change.txt"), []byte(content+"\n"), 0o600)
}

func writeResult(status, files, checks, blockers string) {
	fmt.Println()
	fmt.Println("## RESULT")
	fmt.Printf("status: %s\n", status)
	fmt.Printf("files: %s\n", files)
	fmt.Printf("checks_run: %s\n", checks)
	fmt.Printf("blockers: %s\n", blockers)
}

func emit(event map[string]any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Println(string(encoded))
}
