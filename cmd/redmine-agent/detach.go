package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
)

// submitRun delegates a run to the running `serve` process. `--wait=false` is a
// fire-and-forget scheduler mode: this process cannot keep a pipeline alive
// after it exits, so the long-running worker must own it (contracts/cli.md §run).
func submitRun(ctx context.Context, cfg *config.Config, output *cliOutput, path string, body any) error {
	if cfg == nil {
		return exitWith(exitConfig, "run: configuration is not loaded")
	}
	endpoint, err := loopbackURL(cfg.HTTP.Listen, path)
	if err != nil {
		return exitWith(exitConfig, "run: %v", err)
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return exitWith(exitInternal, "run: encode request: %v", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// #nosec G704 -- the endpoint is derived from the operator's configured http.listen, not request data.
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return exitWith(exitInternal, "run: build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Operator", operatorName())

	client := &http.Client{Timeout: 60 * time.Second}
	// #nosec G704 -- see above: the URL is the configured worker address.
	response, err := client.Do(request)
	if err != nil {
		return exitWith(exitDependency,
			"run: no worker answered at %s; start `redmine-agent serve` first, or drop --wait=false (%v)",
			cfg.HTTP.Listen, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))

	var document map[string]any
	_ = json.Unmarshal(payload, &document)

	if response.StatusCode != http.StatusCreated {
		code := "internal_error"
		detail := strings.TrimSpace(string(payload))
		if value, ok := document["code"].(string); ok {
			code = value
		}
		if value, ok := document["detail"].(string); ok {
			detail = value
		}
		if output.json {
			_ = output.document(map[string]any{
				"code":    code,
				"message": detail,
				"stage":   document["stage"],
				"runId":   document["runId"],
			})
		}
		output.human("refused: %s: %s\n", code, detail)
		switch response.StatusCode {
		case http.StatusBadRequest:
			return exitWith(exitUsage, "run: %s", detail)
		case http.StatusTooManyRequests, http.StatusConflict:
			return exitWith(exitRunFailed, "run: %s", detail)
		default:
			return exitWith(exitInternal, "run: %s", detail)
		}
	}

	if output.json {
		return output.document(document)
	}
	runID, _ := document["runId"].(string)
	output.human("run %s started by the worker at %s\n", runID, cfg.HTTP.Listen)
	return nil
}

// loopbackURL turns a configured bind address into a client URL. Binding to
// 0.0.0.0 or :: is fine for the server; the CLI must dial loopback.
func loopbackURL(listen, path string) (string, error) {
	address := strings.TrimSpace(listen)
	if address == "" {
		return "", fmt.Errorf("http.listen is empty")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("http.listen %q is not host:port: %w", address, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}
