// Package health probes each dependency independently so a degraded component
// is reported rather than thrown (FR-058).
package health

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Status is the observed state of one component.
type Status string

const (
	StatusOK      Status = "ok"
	StatusFailing Status = "failing"
	StatusUnknown Status = "unknown"
)

// ErrNotConfigured marks a probe that cannot run because the dependency is not
// wired into this process. It is reported as "unknown", never as a failure.
var ErrNotConfigured = errors.New("not configured")

// Component is one dependency's result. The JSON shape is the contract's
// HealthReport component.
type Component struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	LatencyMS   int64  `json:"latencyMs"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is one whole check run.
type Report struct {
	Status     string      `json:"status"`
	CheckedAt  time.Time   `json:"checkedAt"`
	Components []Component `json:"components"`
}

// Degraded reports whether any component failed.
func (r Report) Degraded() bool { return r.Status == "degraded" }

// Probe is one named check. A nil Check means "not wired in this process".
type Probe struct {
	Name        string
	Deep        bool
	Remediation string
	Check       func(ctx context.Context) (detail string, err error)
}

// Redactor scrubs text before it reaches a response or a log.
type Redactor interface {
	Redact(string) string
}

// Options configures a Checker.
type Options struct {
	Probes   []Probe
	Timeout  time.Duration
	Now      func() time.Time
	Redactor Redactor
}

// Checker runs probes concurrently, one deadline each, and never returns an
// error: a probe that fails is a component that reports "failing".
type Checker struct {
	probes   []Probe
	timeout  time.Duration
	now      func() time.Time
	redactor Redactor
}

// NewChecker builds a checker from explicit probes, which is what tests use.
func NewChecker(opts Options) *Checker {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Checker{
		probes:   opts.Probes,
		timeout:  timeout,
		now:      now,
		redactor: opts.Redactor,
	}
}

// Run checks every probe. When deep is false, deep probes are reported as
// "unknown" instead of being executed, which is what the fast health path uses.
func (c *Checker) Run(ctx context.Context, deep bool) Report {
	report := Report{Status: string(StatusOK), CheckedAt: c.now()}
	if c == nil {
		report.Status = string(StatusOK)
		return report
	}

	components := make([]Component, len(c.probes))
	var group sync.WaitGroup
	for index, probe := range c.probes {
		index, probe := index, probe
		components[index] = Component{
			Name:        probe.Name,
			Status:      StatusUnknown,
			Remediation: probe.Remediation,
		}
		if probe.Deep && !deep {
			components[index].Detail = "not probed: repeat the request with deep=true"
			continue
		}

		group.Add(1)
		go func() {
			defer group.Done()
			components[index] = c.runOne(ctx, probe)
		}()
	}
	group.Wait()

	for _, component := range components {
		if component.Status == StatusFailing {
			report.Status = "degraded"
		}
	}
	report.Components = components
	return report
}

// RunOne executes a single probe with the same timeout and redaction rules as
// Run. The doctor uses it to keep its documented check order.
func (c *Checker) RunOne(ctx context.Context, probe Probe) Component {
	if c == nil {
		return Component{Name: probe.Name, Status: StatusUnknown, Detail: "no health checker is configured"}
	}
	return c.runOne(ctx, probe)
}

func (c *Checker) runOne(ctx context.Context, probe Probe) Component {
	component := Component{
		Name:        probe.Name,
		Status:      StatusUnknown,
		Remediation: probe.Remediation,
	}
	if probe.Check == nil {
		component.Detail = "this probe is not wired into the running worker"
		return component
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	started := time.Now()
	detail, err := probe.Check(probeCtx)
	component.LatencyMS = time.Since(started).Milliseconds()

	switch {
	case err == nil:
		component.Status = StatusOK
		component.Detail = c.redact(strings.TrimSpace(detail))
		if component.Detail == "" {
			component.Detail = "ok"
		}
	case errors.Is(err, ErrNotConfigured):
		component.Status = StatusUnknown
		component.Detail = c.redact(err.Error())
	default:
		component.Status = StatusFailing
		component.Detail = c.redact(err.Error())
		if strings.TrimSpace(component.Remediation) == "" {
			component.Remediation = "See the worker logs for details."
		}
	}
	return component
}

func (c *Checker) redact(text string) string {
	if c == nil || c.redactor == nil {
		return text
	}
	return c.redactor.Redact(text)
}

// FormatBytes renders a byte count for a human reader.
func FormatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	divisor, exponent := uint64(unit), 0
	for value := bytes / unit; value >= unit; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(divisor), "KMGTPE"[exponent])
}
