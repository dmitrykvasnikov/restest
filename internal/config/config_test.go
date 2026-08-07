package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// env builds a LookupFunc over a literal map, standing in for the process
// environment.
func env(vars map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL": "postgres://u:p@localhost:5432/restest",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q, want http://localhost:8080", cfg.BaseURL)
	}
	// Plain HTTP by default, so a Secure cookie would never come back and
	// local development could not log in at all.
	if cfg.SecureCookies() {
		t.Error("SecureCookies is on for an http base URL")
	}
	if cfg.DatabaseMaxConns != 10 {
		t.Errorf("DatabaseMaxConns = %d, want 10", cfg.DatabaseMaxConns)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.LogBodyLimit != 64*1024 {
		t.Errorf("LogBodyLimit = %d, want 65536", cfg.LogBodyLimit)
	}
	if cfg.LogBuffer != core.DefaultRecorderBuffer {
		t.Errorf("LogBuffer = %d, want %d", cfg.LogBuffer, core.DefaultRecorderBuffer)
	}
	if cfg.LogRetentionMonths != core.DefaultRetentionMonths {
		t.Errorf("LogRetentionMonths = %d, want %d", cfg.LogRetentionMonths, core.DefaultRetentionMonths)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL": "postgres://localhost/db",
		"RESTEST_HTTP_ADDR":    "127.0.0.1:9000",
		// The trailing slash is dropped, so that callers can append a path
		// without checking first.
		"RESTEST_BASE_URL":           "https://restest.example.com/",
		"RESTEST_DATABASE_MAX_CONNS": "42",
		"RESTEST_LOG_LEVEL":          "debug",
		"RESTEST_LOG_FORMAT":         "text",
		"RESTEST_SHUTDOWN_TIMEOUT":   "30s",
		// The request log: how much of a body is kept, how many exchanges may
		// wait to be written, and how many months are kept.
		"RESTEST_LOG_BODY_LIMIT":       "4096",
		"RESTEST_LOG_BUFFER":           "64",
		"RESTEST_LOG_RETENTION_MONTHS": "12",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Config{
		HTTPAddr:           "127.0.0.1:9000",
		BaseURL:            "https://restest.example.com",
		DatabaseURL:        "postgres://localhost/db",
		DatabaseMaxConns:   42,
		LogLevel:           slog.LevelDebug,
		LogFormat:          "text",
		ShutdownTimeout:    30 * time.Second,
		LogBodyLimit:       4096,
		LogBuffer:          64,
		LogRetentionMonths: 12,
	}
	if cfg != want {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
}

// Cookies follow the scheme users actually arrive on, which is the one thing
// the process knows about how it is reached.
func TestSecureCookiesFollowsBaseURL(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL": "postgres://localhost/db",
		"RESTEST_BASE_URL":     "https://restest.example.com",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SecureCookies() {
		t.Error("SecureCookies is off for an https base URL")
	}
}

func TestLoadRejectsBadBaseURL(t *testing.T) {
	for _, bad := range []string{"restest.example.com", "ftp://restest.example.com", "https://", "://x"} {
		t.Run(bad, func(t *testing.T) {
			_, err := Load(env(map[string]string{
				"RESTEST_DATABASE_URL": "postgres://localhost/db",
				"RESTEST_BASE_URL":     bad,
			}))
			if err == nil {
				t.Fatalf("Load accepted %q as a base URL", bad)
			}
			if !strings.Contains(err.Error(), "RESTEST_BASE_URL") {
				t.Errorf("error %q does not name the variable", err)
			}
		})
	}
}

func TestLoadMissingDatabaseURL(t *testing.T) {
	_, err := Load(env(nil))
	if err == nil {
		t.Fatal("Load succeeded without a database URL, want an error")
	}
	if !strings.Contains(err.Error(), "RESTEST_DATABASE_URL") {
		t.Errorf("error %q does not name the missing variable", err)
	}
}

// An empty variable is a misconfiguration, not an intentional empty value:
// `RESTEST_DATABASE_URL=` in a compose file is the same mistake as omitting it.
func TestLoadEmptyValueCountsAsUnset(t *testing.T) {
	_, err := Load(env(map[string]string{"RESTEST_DATABASE_URL": ""}))
	if err == nil {
		t.Fatal("Load accepted an empty database URL, want an error")
	}
}

func TestLoadInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"max conns not a number", "RESTEST_DATABASE_MAX_CONNS", "many"},
		{"max conns zero", "RESTEST_DATABASE_MAX_CONNS", "0"},
		{"max conns negative", "RESTEST_DATABASE_MAX_CONNS", "-1"},
		{"unknown log level", "RESTEST_LOG_LEVEL", "verbose"},
		{"unknown log format", "RESTEST_LOG_FORMAT", "xml"},
		{"shutdown not a duration", "RESTEST_SHUTDOWN_TIMEOUT", "soon"},
		{"shutdown not positive", "RESTEST_SHUTDOWN_TIMEOUT", "0s"},
		// A body limit above the ceiling one row may cost, a buffer of nothing
		// at all, and a retention window that would detach the month being
		// written into.
		{"body limit above the row ceiling", "RESTEST_LOG_BODY_LIMIT", "2097152"},
		{"buffer of zero", "RESTEST_LOG_BUFFER", "0"},
		{"retention below one month", "RESTEST_LOG_RETENTION_MONTHS", "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(map[string]string{
				"RESTEST_DATABASE_URL": "postgres://localhost/db",
				tt.key:                 tt.val,
			}))
			if err == nil {
				t.Fatalf("Load accepted %s=%q, want an error", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name %s", err, tt.key)
			}
		})
	}
}

// Every problem should surface on the first start, not one per restart.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"RESTEST_LOG_LEVEL":  "verbose",
		"RESTEST_LOG_FORMAT": "xml",
	}))
	if err == nil {
		t.Fatal("Load succeeded on a broken environment, want an error")
	}

	for _, key := range []string{"RESTEST_DATABASE_URL", "RESTEST_LOG_LEVEL", "RESTEST_LOG_FORMAT"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s:\n%v", key, err)
		}
	}
}

func TestRedactedDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "password replaced",
			url:  "postgres://restest:s3cret@db:5432/restest?sslmode=disable",
			want: "postgres://restest:xxxxx@db:5432/restest?sslmode=disable",
		},
		{
			name: "no password left alone",
			url:  "postgres://db:5432/restest",
			want: "postgres://db:5432/restest",
		},
		{
			name: "unparseable url is not echoed",
			url:  "postgres://user:pass\x7f@host/db",
			want: "(unparseable)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Config{DatabaseURL: tt.url}.RedactedDatabaseURL()
			if got != tt.want {
				t.Errorf("RedactedDatabaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
