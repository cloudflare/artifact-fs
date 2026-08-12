package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

type credentialStringer string

func (s credentialStringer) String() string { return string(s) }

func TestNewJSONLoggerRedactsEveryStringBoundary(t *testing.T) {
	secret := "https://credential-user:super-secret@example.com/org/repo.git"
	want := auth.RedactString(secret)
	var output bytes.Buffer
	logger := NewJSONLogger(&output, slog.LevelInfo)
	logger.Info(secret,
		"string", secret,
		"error", errors.New(secret),
		"stringer", credentialStringer(secret),
		"attempt", 2,
	)

	for _, exposed := range []string{"credential-user", "super-secret"} {
		if strings.Contains(output.String(), exposed) {
			t.Fatalf("logger exposed %q: %s", exposed, output.String())
		}
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode JSON log: %v\n%s", err, output.String())
	}
	for _, key := range []string{"msg", "string", "error", "stringer"} {
		if got := record[key]; got != want {
			t.Fatalf("%s = %v, want %q", key, got, want)
		}
	}
	if got := record["attempt"]; got != float64(2) {
		t.Fatalf("attempt = %v, want 2", got)
	}
}
