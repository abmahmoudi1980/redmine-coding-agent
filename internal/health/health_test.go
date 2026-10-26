package health_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/health"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

func okProbe(name string) health.Probe {
	return health.Probe{
		Name:        name,
		Remediation: "nothing to do",
		Check: func(context.Context) (string, error) {
			return name + " is fine", nil
		},
	}
}

func failingProbe(name, message, remediation string) health.Probe {
	return health.Probe{
		Name:        name,
		Remediation: remediation,
		Check: func(context.Context) (string, error) {
			return "", errors.New(message)
		},
	}
}

func TestEachProbeReportsIndependently(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	checker := health.NewChecker(health.Options{
		Now: func() time.Time { return now },
		Probes: []health.Probe{
			okProbe("database"),
			failingProbe("redmine", "the credential was rejected", "Check redmine.api_key."),
			okProbe("agent"),
		},
	})

	report := checker.Run(context.Background(), false)

	if report.Status != "degraded" {
		t.Fatalf("report status = %q, want degraded", report.Status)
	}
	if !report.CheckedAt.Equal(now) {
		t.Fatalf("checkedAt = %s, want %s", report.CheckedAt, now)
	}
	if len(report.Components) != 3 {
		t.Fatalf("components = %d, want 3", len(report.Components))
	}

	byName := make(map[string]health.Component, len(report.Components))
	for _, component := range report.Components {
		byName[component.Name] = component
	}
	if byName["database"].Status != health.StatusOK {
		t.Errorf("database status = %q, want ok", byName["database"].Status)
	}
	if byName["agent"].Status != health.StatusOK {
		t.Errorf("agent status = %q, want ok", byName["agent"].Status)
	}
	redmine := byName["redmine"]
	if redmine.Status != health.StatusFailing {
		t.Errorf("redmine status = %q, want failing", redmine.Status)
	}
	if !strings.Contains(redmine.Detail, "credential was rejected") {
		t.Errorf("redmine detail = %q", redmine.Detail)
	}
	if redmine.Remediation == "" {
		t.Error("a failing component must carry remediation text (FR-058)")
	}
	for _, component := range report.Components {
		if component.LatencyMS < 0 {
			t.Errorf("%s latency = %d", component.Name, component.LatencyMS)
		}
	}
}

func TestDeepProbesAreSkippedOnTheFastPath(t *testing.T) {
	deep := okProbe("redmine")
	deep.Deep = true
	checker := health.NewChecker(health.Options{
		Probes: []health.Probe{okProbe("database"), deep},
	})

	report := checker.Run(context.Background(), false)

	if report.Status != "ok" {
		t.Fatalf("report status = %q, want ok", report.Status)
	}
	for _, component := range report.Components {
		if component.Name == "redmine" {
			if component.Status != health.StatusUnknown {
				t.Fatalf("redmine status = %q, want unknown", component.Status)
			}
			if !strings.Contains(component.Detail, "deep=true") {
				t.Fatalf("redmine detail = %q, want a deep-probe hint", component.Detail)
			}
		}
	}
}

func TestProbeDetailIsRedacted(t *testing.T) {
	redactor := logging.NewRedactor()
	redactor.Register("super-secret-value")
	checker := health.NewChecker(health.Options{
		Redactor: redactor,
		Probes: []health.Probe{
			failingProbe("redmine", "request failed for super-secret-value", "rotate the key"),
		},
	})

	report := checker.Run(context.Background(), false)

	if strings.Contains(report.Components[0].Detail, "super-secret-value") {
		t.Fatalf("detail leaked the secret: %q", report.Components[0].Detail)
	}
	if !strings.Contains(report.Components[0].Detail, "***") {
		t.Fatalf("detail = %q, want a redaction marker", report.Components[0].Detail)
	}
}

func TestUnwiredProbeIsUnknownNotFailing(t *testing.T) {
	checker := health.NewChecker(health.Options{
		Probes: []health.Probe{{
			Name:        "repository",
			Deep:        true,
			Remediation: "configure a repository",
			Check: func(context.Context) (string, error) {
				return "", health.ErrNotConfigured
			},
		}},
	})

	report := checker.Run(context.Background(), true)

	if report.Status != "ok" {
		t.Fatalf("report status = %q, want ok", report.Status)
	}
	if report.Components[0].Status != health.StatusUnknown {
		t.Fatalf("status = %q, want unknown", report.Components[0].Status)
	}
}

func TestDefaultProbesReportEachDependency(t *testing.T) {
	checker := health.New(health.Deps{
		FreeSpace: func(string) (uint64, error) { return 4 << 30, nil },
		AgentVersion: func(context.Context) (string, error) {
			return "opencode 1.2.3", nil
		},
	})

	report := checker.Run(context.Background(), true)

	names := make([]string, 0, len(report.Components))
	for _, component := range report.Components {
		names = append(names, component.Name)
	}
	want := []string{"database", "redmine", "gitlab", "repository", "agent", "disk"}
	if len(names) != len(want) {
		t.Fatalf("components = %v, want %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("components = %v, want %v", names, want)
		}
	}
	if report.Components[4].Status != health.StatusOK {
		t.Fatalf("agent status = %q, detail %q", report.Components[4].Status, report.Components[4].Detail)
	}
	if report.Components[5].Status != health.StatusOK {
		t.Fatalf("disk status = %q, detail %q", report.Components[5].Status, report.Components[5].Detail)
	}
	if report.Components[0].Status != health.StatusUnknown {
		t.Fatalf("database without a store = %q, want unknown", report.Components[0].Status)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[uint64]string{
		512:     "512 B",
		2 << 10: "2.0 KiB",
		3 << 30: "3.0 GiB",
	}
	for input, want := range cases {
		if got := health.FormatBytes(input); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", input, got, want)
		}
	}
}
