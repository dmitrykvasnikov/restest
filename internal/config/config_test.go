package config

import (
	"log/slog"
	"net/netip"
	"reflect"
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
	// The demo is on unless an instance says otherwise: serving it to whoever
	// arrives is the point of having it.
	if !cfg.DemoEnabled {
		t.Error("DemoEnabled is off by default")
	}
	if cfg.DemoResetInterval != time.Hour {
		t.Errorf("DemoResetInterval = %v, want 1h", cfg.DemoResetInterval)
	}
	// Hardening defaults. The rate limits are on by default: an instance that
	// wants none says so, rather than being handed none by accident.
	if cfg.RateLimitIP != 50 || cfg.RateLimitProject != 200 || cfg.RateLimitAPI != 20 {
		t.Errorf("rate limits = ip %d, project %d, api %d; want 50, 200, 20",
			cfg.RateLimitIP, cfg.RateLimitProject, cfg.RateLimitAPI)
	}
	// And nobody is believed about a client's address until an operator names
	// the proxy in front of the instance.
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want none by default", cfg.TrustedProxies)
	}
	if cfg.MaxRequestBody != 1024*1024 {
		t.Errorf("MaxRequestBody = %d, want 1048576", cfg.MaxRequestBody)
	}
	if cfg.MaxHeaderBytes != 64*1024 {
		t.Errorf("MaxHeaderBytes = %d, want 65536", cfg.MaxHeaderBytes)
	}
	if cfg.ReadHeaderTimeout != 10*time.Second || cfg.ReadTimeout != 30*time.Second ||
		cfg.WriteTimeout != 30*time.Second || cfg.IdleTimeout != 120*time.Second {
		t.Errorf("timeouts = %v, %v, %v, %v; want 10s, 30s, 30s, 120s",
			cfg.ReadHeaderTimeout, cfg.ReadTimeout, cfg.WriteTimeout, cfg.IdleTimeout)
	}
	if !cfg.MetricsEnabled {
		t.Error("MetricsEnabled is off by default")
	}
	if cfg.MetricsToken != "" {
		t.Errorf("MetricsToken = %q, want empty by default", cfg.MetricsToken)
	}
}

// Zero is how a limit is turned off, and it has to be reachable: the range on
// the setting starts there rather than at one.
func TestARateLimitOfZeroIsAccepted(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL":       "postgres://localhost/db",
		"RESTEST_RATE_LIMIT_IP":      "0",
		"RESTEST_RATE_LIMIT_PROJECT": "0",
		"RESTEST_RATE_LIMIT_API":     "0",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimitIP != 0 || cfg.RateLimitProject != 0 || cfg.RateLimitAPI != 0 {
		t.Errorf("rate limits = %d, %d, %d; want all zero",
			cfg.RateLimitIP, cfg.RateLimitProject, cfg.RateLimitAPI)
	}
}

func TestLoadRejectsANegativeRateLimit(t *testing.T) {
	_, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL":  "postgres://localhost/db",
		"RESTEST_RATE_LIMIT_IP": "-1",
	}))
	if err == nil {
		t.Fatal("Load accepted a negative rate limit")
	}
	if !strings.Contains(err.Error(), "RESTEST_RATE_LIMIT_IP") {
		t.Errorf("error %q does not name the variable", err)
	}
}

func TestTrustedProxiesAreParsedOrRefused(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		cfg, err := Load(env(map[string]string{
			"RESTEST_DATABASE_URL": "postgres://localhost/db",
			// Spaces, an address, a block, a v6 block, and a stray empty field
			// from a trailing comma — all of which a compose file produces.
			"RESTEST_TRUSTED_PROXIES": " 172.17.0.1 , 10.0.0.0/8, fd00::/8, ",
		}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		want := []netip.Prefix{
			netip.MustParsePrefix("172.17.0.1/32"),
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("fd00::/8"),
		}
		if !reflect.DeepEqual(cfg.TrustedProxies, want) {
			t.Errorf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
		}
	})

	t.Run("masked", func(t *testing.T) {
		// A block written with host bits set means the block, not a prefix that
		// would then fail to contain the address it was written from.
		cfg, err := Load(env(map[string]string{
			"RESTEST_DATABASE_URL":    "postgres://localhost/db",
			"RESTEST_TRUSTED_PROXIES": "10.4.3.2/8",
		}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.TrustedProxiesString(); got != "10.0.0.0/8" {
			t.Errorf("TrustedProxies = %s, want 10.0.0.0/8", got)
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, bad := range []string{"not-an-ip", "10.0.0.0/64", "localhost", "10.0.0.1-10.0.0.9"} {
			t.Run(bad, func(t *testing.T) {
				_, err := Load(env(map[string]string{
					"RESTEST_DATABASE_URL":    "postgres://localhost/db",
					"RESTEST_TRUSTED_PROXIES": bad,
				}))
				if err == nil {
					t.Fatalf("Load accepted %q as a trusted proxy", bad)
				}
				if !strings.Contains(err.Error(), "RESTEST_TRUSTED_PROXIES") {
					t.Errorf("error %q does not name the variable", err)
				}
			})
		}
	})
}

// The startup log carries the whole configuration, and a metrics token in it is
// a token in whatever aggregates the log.
func TestTheMetricsTokenIsNeverLogged(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL":  "postgres://u:secret@localhost/db",
		"RESTEST_METRICS_TOKEN": "hunter2",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	logged := cfg.LogValue().String()
	if strings.Contains(logged, "hunter2") {
		t.Errorf("the metrics token is in the log line: %s", logged)
	}
	if !strings.Contains(logged, "metrics_token_set=true") {
		t.Errorf("the log line does not say the token is set: %s", logged)
	}
	if strings.Contains(logged, "secret") {
		t.Errorf("the database password is in the log line: %s", logged)
	}
}

// The reset interval has a floor because of what the setting means rather than
// what it costs: a demo reset every second discards a visitor's POST before
// they can GET it back, which is the one thing the demo exists to show.
func TestLoadRejectsAnAbsurdDemoResetInterval(t *testing.T) {
	for _, bad := range []string{"1s", "30s", "1000h", "0s", "-1m", "soon"} {
		t.Run(bad, func(t *testing.T) {
			_, err := Load(env(map[string]string{
				"RESTEST_DATABASE_URL":        "postgres://localhost/db",
				"RESTEST_DEMO_RESET_INTERVAL": bad,
			}))
			if err == nil {
				t.Fatalf("Load accepted %q as the demo reset interval", bad)
			}
			if !strings.Contains(err.Error(), "RESTEST_DEMO_RESET_INTERVAL") {
				t.Errorf("error %q does not name the variable", err)
			}
		})
	}
}

func TestLoadRejectsANonBooleanDemoFlag(t *testing.T) {
	_, err := Load(env(map[string]string{
		"RESTEST_DATABASE_URL": "postgres://localhost/db",
		"RESTEST_DEMO_ENABLED": "yes please",
	}))
	if err == nil {
		t.Fatal("Load accepted a non-boolean RESTEST_DEMO_ENABLED")
	}
	if !strings.Contains(err.Error(), "RESTEST_DEMO_ENABLED") {
		t.Errorf("error %q does not name the variable", err)
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
		// The demo project: an instance that does not want to serve one says so.
		"RESTEST_DEMO_ENABLED":        "false",
		"RESTEST_DEMO_RESET_INTERVAL": "15m",
		// Hardening: the server's own timeouts, the caps on what one request may
		// be, the rate limits and who may be believed about a client's address.
		"RESTEST_READ_HEADER_TIMEOUT": "5s",
		"RESTEST_READ_TIMEOUT":        "20s",
		"RESTEST_WRITE_TIMEOUT":       "25s",
		"RESTEST_IDLE_TIMEOUT":        "60s",
		"RESTEST_MAX_HEADER_BYTES":    "16384",
		"RESTEST_MAX_REQUEST_BODY":    "524288",
		"RESTEST_RATE_LIMIT_IP":       "5",
		"RESTEST_RATE_LIMIT_PROJECT":  "25",
		"RESTEST_RATE_LIMIT_API":      "2",
		"RESTEST_TRUSTED_PROXIES":     "10.0.0.0/8, 192.168.1.7",
		"RESTEST_METRICS_ENABLED":     "false",
		"RESTEST_METRICS_TOKEN":       "scrape-me",
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
		DemoEnabled:        false,
		DemoResetInterval:  15 * time.Minute,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        20 * time.Second,
		WriteTimeout:       25 * time.Second,
		IdleTimeout:        60 * time.Second,
		MaxHeaderBytes:     16384,
		MaxRequestBody:     524288,
		RateLimitIP:        5,
		RateLimitProject:   25,
		RateLimitAPI:       2,
		TrustedProxies: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			// A bare address becomes the single-address block it means.
			netip.MustParsePrefix("192.168.1.7/32"),
		},
		MetricsEnabled: false,
		MetricsToken:   "scrape-me",
	}
	if !reflect.DeepEqual(cfg, want) {
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
