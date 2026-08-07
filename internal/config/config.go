// Package config loads the application configuration from the environment.
//
// The whole configuration is read once, at startup, and every problem found is
// reported together. A process that starts half-configured and only fails when
// it first touches the broken setting is much harder to diagnose than one that
// refuses to start and says exactly what is wrong.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// Prefix is prepended to every environment variable name.
const Prefix = "RESTEST_"

// Config is the fully validated configuration of a restest process.
type Config struct {
	// HTTPAddr is the listen address of the HTTP server, as accepted by net.Listen.
	HTTPAddr string
	// BaseURL is the address the outside world reaches this instance on, with
	// no trailing slash. It is what the UI shows users as the root of their
	// mock URLs, and its scheme decides whether cookies are marked Secure —
	// which is why one setting covers both rather than two that can disagree.
	BaseURL string
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// DatabaseMaxConns caps the pgx connection pool.
	DatabaseMaxConns int32
	// LogLevel is the minimum level written to the log.
	LogLevel slog.Level
	// LogFormat selects the slog handler: "json" for deployments, "text" for a
	// readable local terminal.
	LogFormat string
	// ShutdownTimeout bounds how long a graceful shutdown waits for in-flight
	// requests before the process exits anyway.
	ShutdownTimeout time.Duration

	// LogBodyLimit is how much of a request or response body the request log
	// keeps, in bytes. Bodies above it are stored truncated and marked as such,
	// so the inspector never implies it is showing the whole thing.
	LogBodyLimit int
	// LogBuffer is how many recorded exchanges may wait to be written. Beyond
	// it they are dropped and counted rather than made to wait, because a
	// request should never be slowed down by the log of it.
	LogBuffer int
	// LogRetentionMonths is how many months of request log are kept, counting
	// the current one. Expiry is a partition detached and dropped.
	LogRetentionMonths int

	// DemoEnabled provisions the shared demo project at startup and resets it on
	// a schedule. Off, nothing is created and the login page stops offering it;
	// a demo already in the database is left alone rather than deleted.
	DemoEnabled bool
	// DemoResetInterval is how often the demo project is restored to its seeds,
	// so that one visitor's writes cannot spoil it for the next.
	DemoResetInterval time.Duration
}

// Bounds on the demo reset interval. The floor is not about correctness — a
// reset is one transaction per collection — but about what the setting means: a
// demo reset every few seconds is a demo that discards a visitor's POST before
// they can GET it back, which is the one thing the demo is there to show.
const (
	minDemoResetInterval = time.Minute
	maxDemoResetInterval = 7 * 24 * time.Hour
)

// LookupFunc reads one environment variable. It has the signature of
// os.LookupEnv so that tests can supply a fake environment.
type LookupFunc func(key string) (string, bool)

// Load reads and validates the configuration. All errors are returned joined
// together, so a misconfigured deployment is fixed in one pass rather than one
// restart per mistake.
func Load(lookup LookupFunc) (Config, error) {
	l := loader{lookup: lookup}

	cfg := Config{
		HTTPAddr:         l.str("HTTP_ADDR", ":8080"),
		BaseURL:          l.httpURL("BASE_URL", "http://localhost:8080"),
		DatabaseURL:      l.requiredStr("DATABASE_URL"),
		DatabaseMaxConns: int32(l.intVal("DATABASE_MAX_CONNS", 10, 1, 1000)),
		LogLevel:         l.level("LOG_LEVEL", slog.LevelInfo),
		LogFormat:        l.oneOf("LOG_FORMAT", "json", "json", "text"),
		ShutdownTimeout:  l.duration("SHUTDOWN_TIMEOUT", 15*time.Second),

		// The request log. The body limit is a decision about what is worth
		// keeping rather than what is safe to accept: mock request bodies are
		// capped at 1 MiB on the way in, and 64 KiB of that covers the JSON
		// payloads a REST client actually sends while bounding what a month of
		// traffic costs. A body above it is stored truncated and marked.
		LogBodyLimit:       l.intVal("LOG_BODY_LIMIT", 64*1024, 0, core.MaxExchangeBody),
		LogBuffer:          l.intVal("LOG_BUFFER", core.DefaultRecorderBuffer, 1, 1_000_000),
		LogRetentionMonths: l.intVal("LOG_RETENTION_MONTHS", core.DefaultRetentionMonths, core.MinRetentionMonths, 120),

		// The demo project. On by default: an instance that serves the demo to
		// anyone who arrives is the point of it, and the way to have a private
		// instance is to say so rather than to be given one by accident.
		DemoEnabled: l.boolean("DEMO_ENABLED", true),
		DemoResetInterval: l.durationRange("DEMO_RESET_INTERVAL", time.Hour,
			minDemoResetInterval, maxDemoResetInterval),
	}
	if err := l.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// SecureCookies reports whether session and CSRF cookies should carry the
// Secure attribute. It follows the scheme of BaseURL, because a Secure cookie
// is never sent over plain HTTP: hard-coding it true would silently break every
// local development login, and hard-coding it false would ship that breakage to
// production instead.
func (c Config) SecureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

// RedactedDatabaseURL is the connection string with its password replaced, so
// that the configuration can be logged at startup. A URL that cannot be parsed
// is reported as such rather than echoed, since it may still contain a secret.
func (c Config) RedactedDatabaseURL() string {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "(unparseable)"
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

// LogValue implements slog.LogValuer so that logging a Config can never leak
// the database password.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("base_url", c.BaseURL),
		slog.Bool("secure_cookies", c.SecureCookies()),
		slog.String("database_url", c.RedactedDatabaseURL()),
		slog.Int("database_max_conns", int(c.DatabaseMaxConns)),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("log_format", c.LogFormat),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Int("log_body_limit", c.LogBodyLimit),
		slog.Int("log_buffer", c.LogBuffer),
		slog.Int("log_retention_months", c.LogRetentionMonths),
		slog.Bool("demo_enabled", c.DemoEnabled),
		slog.Duration("demo_reset_interval", c.DemoResetInterval),
	)
}

// loader reads individual variables, accumulating problems instead of failing
// on the first one.
type loader struct {
	lookup LookupFunc
	errs   []error
}

func (l *loader) err() error { return errors.Join(l.errs...) }

func (l *loader) fail(key string, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s%s: %s", Prefix, key, fmt.Sprintf(format, args...)))
}

// raw returns the trimmed value and whether it was set to something non-empty.
// An empty variable is treated as unset: in compose files and shell scripts an
// unset variable and one set to "" are the same mistake.
func (l *loader) raw(key string) (string, bool) {
	v, ok := l.lookup(Prefix + key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) requiredStr(key string) string {
	v, ok := l.raw(key)
	if !ok {
		l.fail(key, "is required")
	}
	return v
}

func (l *loader) intVal(key string, def, min, max int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, "%q is not a number", v)
		return def
	}
	if n < min || n > max {
		l.fail(key, "%d is out of range [%d, %d]", n, min, max)
		return def
	}
	return n
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, "%q is not a duration such as 15s or 2m", v)
		return def
	}
	if d <= 0 {
		l.fail(key, "%s must be positive", d)
		return def
	}
	return d
}

// durationRange is duration with bounds, for a setting where both a value too
// small and a value too large are mistakes worth naming rather than accepting.
func (l *loader) durationRange(key string, def, min, max time.Duration) time.Duration {
	d := l.duration(key, def)
	if d < min || d > max {
		l.fail(key, "%s is out of range [%s, %s]", d, min, max)
		return def
	}
	return d
}

// boolean accepts what strconv.ParseBool does — true/false, 1/0, t/f, and the
// capitalised forms — because those are what a compose file and a shell both
// end up producing.
func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, "%q is not true or false", v)
		return def
	}
	return b
}

func (l *loader) level(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, "%q is not a level (debug, info, warn, error)", v)
		return def
	}
	return lvl
}

// httpURL accepts an absolute http or https URL and returns it without a
// trailing slash, so that callers can append a path without doubling it.
func (l *loader) httpURL(key, def string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		l.fail(key, "%q is not an absolute http(s) URL such as https://restest.example.com", v)
		return def
	}
	return strings.TrimRight(u.String(), "/")
}

func (l *loader) oneOf(key, def string, allowed ...string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(key, "%q is not one of %v", v, allowed)
	return def
}
