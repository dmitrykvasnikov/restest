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
	"net/netip"
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

	// ReadHeaderTimeout bounds how long a client may take to send its request
	// line and headers. It is the guard against a connection opened and then
	// left to dawdle, which costs the server a goroutine and the client nothing.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the whole request, headers and body together.
	ReadTimeout time.Duration
	// WriteTimeout bounds the response. An endpoint configured with a delay
	// lifts it for its own response through http.ResponseController, which is
	// better than weakening the guard on every route to accommodate a few.
	WriteTimeout time.Duration
	// IdleTimeout bounds how long a kept-alive connection may sit between
	// requests.
	IdleTimeout time.Duration
	// MaxHeaderBytes caps the request line and headers.
	MaxHeaderBytes int
	// MaxRequestBody caps every request body, applied before any handler reads
	// one. A mock server is by design something people point half-finished
	// clients at, and without a cap one request can ask this process to hold as
	// much memory as the client cares to send.
	MaxRequestBody int

	// RateLimitIP is how many mock requests per second one client address may
	// make, across every project. Zero turns the limit off.
	RateLimitIP int
	// RateLimitProject is how many mock requests per second one project may
	// serve, across every client. Zero turns the limit off.
	RateLimitProject int
	// RateLimitAPI is how many management API requests per second one
	// credential may make. Zero turns the limit off.
	RateLimitAPI int
	// TrustedProxies are the peers whose X-Forwarded-For header is believed.
	// Empty — the default — means the header is never believed and the client
	// address is always the peer's, which is the only safe answer for an
	// instance reached directly.
	TrustedProxies []netip.Prefix

	// MetricsEnabled serves the Prometheus exposition at /metrics.
	MetricsEnabled bool
	// MetricsToken, when set, is the bearer token /metrics requires. Empty
	// leaves the endpoint open to anyone who can reach it, which is right
	// behind a proxy that does not route it and wrong on an instance published
	// as it stands.
	MetricsToken string
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

		// Server timeouts. The defaults are generous enough that no honest
		// client meets them and short enough that a connection nobody is using
		// is reclaimed. They are settings rather than constants because the one
		// deployment that legitimately needs a longer read is the one nobody
		// anticipated.
		ReadHeaderTimeout: l.durationRange("READ_HEADER_TIMEOUT", 10*time.Second, time.Second, time.Hour),
		ReadTimeout:       l.durationRange("READ_TIMEOUT", 30*time.Second, time.Second, time.Hour),
		WriteTimeout:      l.durationRange("WRITE_TIMEOUT", 30*time.Second, time.Second, time.Hour),
		IdleTimeout:       l.durationRange("IDLE_TIMEOUT", 120*time.Second, time.Second, time.Hour),
		MaxHeaderBytes:    l.intVal("MAX_HEADER_BYTES", 64*1024, 4*1024, 1024*1024),
		MaxRequestBody:    l.intVal("MAX_REQUEST_BODY", 1024*1024, 4*1024, 64*1024*1024),

		// Rate limits, in requests per second per key. The defaults are set
		// where a test suite hammering a mock never notices them and a runaway
		// loop does: a client sending fifty mock requests a second is already
		// far past what a CI job needs from a fixture server.
		RateLimitIP:      l.intVal("RATE_LIMIT_IP", 50, 0, 1_000_000),
		RateLimitProject: l.intVal("RATE_LIMIT_PROJECT", 200, 0, 1_000_000),
		RateLimitAPI:     l.intVal("RATE_LIMIT_API", 20, 0, 1_000_000),
		TrustedProxies:   l.prefixes("TRUSTED_PROXIES"),

		MetricsEnabled: l.boolean("METRICS_ENABLED", true),
		MetricsToken:   l.str("METRICS_TOKEN", ""),
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
		slog.Duration("read_header_timeout", c.ReadHeaderTimeout),
		slog.Duration("read_timeout", c.ReadTimeout),
		slog.Duration("write_timeout", c.WriteTimeout),
		slog.Duration("idle_timeout", c.IdleTimeout),
		slog.Int("max_header_bytes", c.MaxHeaderBytes),
		slog.Int("max_request_body", c.MaxRequestBody),
		slog.Int("rate_limit_ip", c.RateLimitIP),
		slog.Int("rate_limit_project", c.RateLimitProject),
		slog.Int("rate_limit_api", c.RateLimitAPI),
		slog.String("trusted_proxies", c.TrustedProxiesString()),
		slog.Bool("metrics_enabled", c.MetricsEnabled),
		// Whether the endpoint is guarded, never by what. A token in the
		// startup log is a token in the log aggregator.
		slog.Bool("metrics_token_set", c.MetricsToken != ""),
	)
}

// TrustedProxiesString renders the trusted list for the startup log, where
// "none" is a more readable answer than an empty bracket pair — and is the
// answer an operator debugging a wrong client address most needs to see.
func (c Config) TrustedProxiesString() string {
	if len(c.TrustedProxies) == 0 {
		return "none"
	}

	parts := make([]string, len(c.TrustedProxies))
	for i, p := range c.TrustedProxies {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
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

// prefixes reads a comma-separated list of IP addresses and CIDR blocks — the
// peers whose X-Forwarded-For header is worth believing.
//
// A bare address is accepted as well as a block, because "the one proxy in
// front of this instance" is the common case and writing /32 after it is a
// detail to get wrong rather than a decision to make. Unset means an empty
// list, which means the header is never believed.
func (l *loader) prefixes(key string) []netip.Prefix {
	v, ok := l.raw(key)
	if !ok {
		return nil
	}

	var out []netip.Prefix
	for _, field := range strings.Split(v, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		if prefix, err := netip.ParsePrefix(field); err == nil {
			// Masked, so that 10.0.0.7/8 means the block the author meant
			// rather than a prefix netip.Prefix.Contains would refuse to match.
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			l.fail(key, "%q is not an IP address or CIDR block such as 10.0.0.0/8", field)
			continue
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out
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
