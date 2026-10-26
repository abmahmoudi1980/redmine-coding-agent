package logging_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/config"
	"github.com/systemgroup/redmine-coding-agent/internal/logging"
)

func TestRedactCoversCommonEncodings(t *testing.T) {
	secret := `tok"en\a b&c=d`
	redactor := logging.NewRedactor()
	redactor.Register(secret)

	jsonBody, err := json.Marshal(map[string]string{"token": secret})
	if err != nil {
		t.Fatal(err)
	}
	samples := []string{
		"plain " + secret + " text",
		string(jsonBody),
		"%22token%22%3A%22" + url.QueryEscape(secret) + "%22",
		url.PathEscape(secret),
	}
	for _, sample := range samples {
		redacted := redactor.Redact(sample)
		if strings.Contains(redacted, secret) {
			t.Errorf("raw secret survived in %q", redacted)
		}
		if !strings.Contains(redacted, "***") {
			t.Errorf("no replacement marker in %q", redacted)
		}
	}
}

func TestRedactLongestFirst(t *testing.T) {
	redactor := logging.NewRedactor()
	redactor.Register("abc", "abcdef")
	if got := redactor.Redact("abcdef"); got != "***" {
		t.Fatalf("got %q, want ***", got)
	}
}

func TestWriterHoldsPartialLines(t *testing.T) {
	redactor := logging.NewRedactor()
	redactor.Register("topsecret")
	var buffer bytes.Buffer
	writer := redactor.Writer(&buffer)

	if _, err := writer.Write([]byte("before top")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("secret after\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buffer.String(), "topsecret") {
		t.Fatalf("secret leaked: %q", buffer.String())
	}
	if !strings.Contains(buffer.String(), "before *** after") {
		t.Fatalf("unexpected output: %q", buffer.String())
	}
}

func TestSecretNeverSurvivesFormattingOrSlog(t *testing.T) {
	const secret = "s3cr3t-token-value"
	t.Setenv("REDMINE_TEST_KEY", secret)
	ref := config.NewSecretRef("env:REDMINE_TEST_KEY")

	value, err := ref.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	formatted := fmt.Sprintf("%v %s %+v %#v", ref, ref, ref, ref)
	if strings.Contains(formatted, value) {
		t.Fatalf("SecretRef leaked through fmt: %s", formatted)
	}

	redactor := logging.NewRedactor()
	redactor.Register(value)
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(redactor.Writer(&buffer), nil))
	logger.Info("authenticating", "token", value, "secret_ref", ref)
	output := buffer.String()
	if strings.Contains(output, value) {
		t.Fatalf("slog output leaked the secret: %s", output)
	}
	if !strings.Contains(output, "env:REDMINE_TEST_KEY") {
		t.Fatalf("reference missing from slog output: %s", output)
	}
}
