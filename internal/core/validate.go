package core

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/mail"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits on user-supplied text. The slug rule is the database constraint
// `projects_slug_format` written out in Go: the two must agree, because a
// string that passes here and fails there surfaces as a 500 instead of as a
// message next to the field.
const (
	maxEmailLen = 254 // RFC 5321 §4.5.3.1.3, the longest a forward path may be
	// NIST SP 800-63B asks for a minimum of 8 and no composition rules. The
	// upper bound is not a security property, only a limit on how much work one
	// request can ask Argon2id to do.
	minPasswordLen = 8
	maxPasswordLen = 256
	maxNameLen     = 80
	maxSlugLen     = 40
)

// slugPattern mirrors the check constraint in migration 00001. Lower case,
// digits and hyphens; must start and end alphanumeric; 1 to 40 characters.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// reservedSlugs are refused even though the database would accept them.
//
// Mock traffic lives under /m/{slug}/, so a slug cannot actually collide with
// an application route today (DESIGN.md §4). The list exists because subdomain
// addressing is an explicitly allowed future alias, and a slug that is fine now
// but breaks then is a migration nobody wants to run. `demo` is held for the
// shared demo project of M5.
var reservedSlugs = []string{"api", "admin", "demo", "healthz", "m", "static"}

// NormalizeEmail puts an address into the form it is stored and compared in.
// The column is citext, so case is already insensitive; this trims the
// whitespace that arrives from a copy-paste and keeps the stored value tidy.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validateEmail accepts a bare address and nothing else. mail.ParseAddress also
// accepts `Name <a@b>`, which is not what a login form means, so the parse has
// to agree that the whole input was the address.
func validateEmail(fe *FieldErrors, email string) {
	switch {
	case email == "":
		fe.Add("email", "Enter your email address.")
	case len(email) > maxEmailLen:
		fe.Add("email", "That address is too long.")
	default:
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email || strings.Count(email, "@") != 1 {
			fe.Add("email", "That does not look like an email address.")
		}
	}
}

func validatePassword(fe *FieldErrors, password string) {
	switch {
	case password == "":
		fe.Add("password", "Choose a password.")
	// Counted in runes: a passphrase of eight characters is eight characters
	// whether or not they are ASCII.
	case utf8.RuneCountInString(password) < minPasswordLen:
		fe.Add("password", "Use at least 8 characters.")
	case len(password) > maxPasswordLen:
		fe.Add("password", "That password is too long.")
	}
}

// validateSlug enforces the database constraint and the reserved list.
func validateSlug(fe *FieldErrors, slug string) {
	switch {
	case slug == "":
		fe.Add("slug", "Choose a slug — it is the name in the mock URL.")
	case len(slug) > maxSlugLen:
		fe.Add("slug", "Use 40 characters or fewer.")
	case !slugPattern.MatchString(slug):
		fe.Add("slug", "Use lower-case letters, digits and hyphens, starting and ending with a letter or digit.")
	case slices.Contains(reservedSlugs, slug):
		fe.Add("slug", "That slug is reserved. Pick another one.")
	}
}

func validateProjectName(fe *FieldErrors, name string) {
	switch {
	case name == "":
		fe.Add("name", "Give the project a name.")
	case utf8.RuneCountInString(name) > maxNameLen:
		fe.Add("name", "Use 80 characters or fewer.")
	case strings.ContainsFunc(name, isControl):
		fe.Add("name", "Remove the control characters.")
	}
}

// isControl catches the characters that would corrupt a log line or a template
// rather than describe a project.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// Limits on an endpoint definition. The delay ceiling is the database's
// `endpoints_delay_sane` constraint written out in Go, for the same reason the
// slug pattern is: a value that passes here and fails there is a 500 where a
// message beside the field belongs.
const (
	maxPathLen        = 512
	maxPathSegments   = 32
	maxBodyLen        = 1 << 20 // 1 MiB
	maxHeaders        = 32
	maxHeaderValueLen = 2048
	minStatusCode     = 100
	maxStatusCode     = 599
	maxDelayMS        = 60_000
)

// Methods are the verbs an endpoint may answer, in the order they are offered
// in the form. They mirror the `endpoints_method_valid` constraint.
var Methods = []string{
	"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", MethodAny,
}

// paramPattern is one whole path segment that is a named parameter. The name
// rules are deliberately narrow — a parameter is read back out as a Go map key
// and printed in a 404 body, and there is no reason for it to be exotic.
var paramPattern = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// forbiddenHeaders are response headers an endpoint may not set. Every one of
// them belongs to the connection rather than to the response: letting a mock
// definition write Content-Length or Transfer-Encoding would let it desynchronise
// its own framing, which is a broken response at best.
var forbiddenHeaders = []string{
	"Connection", "Content-Length", "Keep-Alive", "Proxy-Connection",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// NormalizeMethod puts a verb into the form it is stored and matched in.
func NormalizeMethod(method string) string {
	return strings.ToUpper(strings.TrimSpace(method))
}

// NormalizePath is the canonical form of a path pattern, and the only form
// stored: a leading slash, no repeated slashes, and no trailing slash except at
// the root.
//
// Normalising before storing is what makes the unique index on
// (project, method, path) mean what it says. Otherwise `/users` and `/users/`
// would be two rows that match the same requests, and which one answered would
// be a coin toss.
func NormalizePath(path string) string {
	segments := SplitPath(path)
	if len(segments) == 0 {
		return "/"
	}
	return "/" + strings.Join(segments, "/")
}

// SplitPath breaks an already-decoded path into its segments, dropping the
// empty ones that leading, trailing and doubled slashes produce.
//
// Both the stored pattern and the incoming request go through this, which is
// what makes `/users`, `/users/` and `//users` the same route rather than three
// near-misses a user has to debug.
func SplitPath(path string) []string {
	path = strings.TrimSpace(path)

	segments := make([]string, 0, strings.Count(path, "/")+1)
	for segment := range strings.SplitSeq(path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

// PathParams returns the names of the parameters in a pattern, in the order
// they appear. The matcher zips these with the values it collected, which is
// how two patterns can share a position with different names for it.
func PathParams(pattern string) []string {
	var names []string
	for _, segment := range SplitPath(pattern) {
		if paramPattern.MatchString(segment) {
			names = append(names, segment[1:len(segment)-1])
		}
	}
	return names
}

func validateMethod(fe *FieldErrors, method string) {
	if !slices.Contains(Methods, method) {
		fe.Add("method", "Choose one of the listed methods.")
	}
}

// validatePath enforces the shape the matcher can actually build a trie from.
//
// A parameter has to be a whole segment: `/v{n}` is a plausible thing to type
// and is not supported, so it is refused with a message saying so rather than
// quietly matching the literal text `v{n}`.
func validatePath(fe *FieldErrors, path string) {
	segments := SplitPath(path)

	switch {
	case len(path) > maxPathLen:
		fe.Add("path", fmt.Sprintf("Use %d characters or fewer.", maxPathLen))
		return
	case len(segments) > maxPathSegments:
		fe.Add("path", fmt.Sprintf("Use %d path segments or fewer.", maxPathSegments))
		return
	case strings.ContainsFunc(path, isControl):
		fe.Add("path", "Remove the control characters.")
		return
	case strings.ContainsAny(path, "?#"):
		fe.Add("path", "Leave the query string out — only the path is matched.")
		return
	}

	seen := make(map[string]bool, len(segments))
	for _, segment := range segments {
		switch {
		case paramPattern.MatchString(segment):
			name := segment[1 : len(segment)-1]
			if seen[name] {
				fe.Add("path", fmt.Sprintf("The parameter {%s} appears twice.", name))
				return
			}
			seen[name] = true
		case strings.ContainsAny(segment, "{}"):
			fe.Add("path", "A parameter has to be a whole segment, like /users/{id}.")
			return
		}
	}
}

func validateKind(fe *FieldErrors, kind string) {
	if !slices.Contains(Kinds, kind) {
		fe.Add("kind", "Choose one of the listed kinds.")
	}
}

// collectionIDParam is the parameter name the expansion of a collection
// endpoint appends to its path — /users becomes /users/{id} for the routes that
// address a single document.
const collectionIDParam = "id"

// validateCollectionPath refuses a root path that would collide with the
// parameter the expansion adds.
//
// The path is a root, not a route: a collection endpoint at /users answers six
// requests across /users and /users/{id}. Parameters *are* allowed before that
// point — /tenants/{tenant}/users is a legitimate place to root a collection —
// but they name nothing the collection reads, because state is per collection
// and not per parameter value. That is the shared-state decision in
// DESIGN.md §12.1, and it is what per-run isolation would later change.
func validateCollectionPath(fe *FieldErrors, path string) {
	if slices.Contains(PathParams(path), collectionIDParam) {
		fe.Add("path", fmt.Sprintf(
			"A collection path cannot itself declare {%s}: the routes for a single document add it.",
			collectionIDParam))
	}
}

func validateStatusCode(fe *FieldErrors, status int) {
	if status < minStatusCode || status > maxStatusCode {
		fe.Add("status_code", fmt.Sprintf("Use a status between %d and %d.", minStatusCode, maxStatusCode))
	}
}

func validateDelay(fe *FieldErrors, delayMS int) {
	if delayMS < 0 || delayMS > maxDelayMS {
		fe.Add("delay_ms", fmt.Sprintf("Use a delay between 0 and %d milliseconds.", maxDelayMS))
	}
}

func validateBody(fe *FieldErrors, body string) {
	if len(body) > maxBodyLen {
		fe.Add("body", "That body is larger than 1 MiB. Serve something that size from a real server.")
	}
}

// validateHeaders checks the response headers parsed out of the form's
// "Name: value" lines.
func validateHeaders(fe *FieldErrors, headers Headers) {
	if len(headers) > maxHeaders {
		fe.Add("headers", fmt.Sprintf("Set %d headers or fewer.", maxHeaders))
		return
	}

	// Sorted, so the message names the same header every time rather than
	// whichever one the map happened to yield first.
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		value := headers[name]
		switch {
		case !validHeaderName(name):
			fe.Add("headers", fmt.Sprintf("%q is not a header name. Write one header per line, as Name: value.", name))
		case slices.Contains(forbiddenHeaders, name):
			fe.Add("headers", fmt.Sprintf("%s is set by the server and cannot be overridden.", name))
		case len(value) > maxHeaderValueLen:
			fe.Add("headers", fmt.Sprintf("The value of %s is too long.", name))
		case strings.ContainsAny(value, "\r\n"):
			fe.Add("headers", fmt.Sprintf("Remove the line breaks from the value of %s.", name))
		default:
			continue
		}
		return
	}
}

// Limits on a collection definition.
const (
	// defaultIDField is the field a collection carries its identifier in unless
	// it is told otherwise. It is what almost every REST API uses, and a
	// collection that accepts the default needs no decision made about it.
	defaultIDField = "id"

	maxCollectionNameLen = 40
	maxIDFieldLen        = 40
	// A seed is a fixture, not a database. The cap is generous enough for the
	// datasets people actually mock and small enough that a collection page
	// still renders.
	maxSeedBytes    = 1 << 20 // 1 MiB
	maxSeedElements = 5000
)

// collectionNamePattern is the shape of a collection name. It is deliberately
// close to the slug rule — a collection name appears in the reset URL, and
// anything needing percent-encoding there would be a name people get wrong. The
// underscore is allowed because `line_items` is how these things are written.
var collectionNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,38}[a-z0-9])?$`)

// idFieldPattern is the shape of the JSON key carrying the identifier. Narrow
// on purpose: the field is read back out of a document, written into URLs and
// used as a filter name, and there is no reason for it to be exotic.
var idFieldPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,39}$`)

// NormalizeCollectionName puts a name into the form it is stored and addressed
// in. Lower case, because the reset URL carries it and a URL that works in one
// capitalisation and not another is a bug waiting to be reported.
func NormalizeCollectionName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func validateCollectionName(fe *FieldErrors, name string) {
	switch {
	case name == "":
		fe.Add("name", "Give the collection a name — it is what appears in the URL.")
	case len(name) > maxCollectionNameLen:
		fe.Add("name", fmt.Sprintf("Use %d characters or fewer.", maxCollectionNameLen))
	case !collectionNamePattern.MatchString(name):
		fe.Add("name", "Use lower-case letters, digits, hyphens and underscores, starting and ending with a letter or digit.")
	}
}

func validateIDField(fe *FieldErrors, field string) {
	switch {
	case field == "":
		fe.Add("id_field", "Name the field that carries the identifier.")
	case len(field) > maxIDFieldLen:
		fe.Add("id_field", fmt.Sprintf("Use %d characters or fewer.", maxIDFieldLen))
	case !idFieldPattern.MatchString(field):
		fe.Add("id_field", "Use letters, digits, hyphens and underscores, starting with a letter or an underscore.")
	}
}

func validateIDStrategy(fe *FieldErrors, strategy string) {
	if !slices.Contains(IDStrategies, strategy) {
		fe.Add("id_strategy", "Choose one of the listed strategies.")
	}
}

// validateSeed checks the seed as text, so that what is refused is what was
// typed. The database would refuse a non-array too, through
// `collections_seed_is_array`, but as a check violation rather than as a
// sentence beside the editor.
func validateSeed(fe *FieldErrors, seed string) {
	if len(seed) > maxSeedBytes {
		fe.Add("seed", "That seed is larger than 1 MiB. A fixture that big belongs in a real database.")
		return
	}

	// A JSON null unmarshals into a slice without being an array, so it would
	// pass the check below and then be refused by the column's
	// `collections_seed_is_array` constraint — a 500 where a sentence beside
	// the field belongs.
	if strings.TrimSpace(seed) == "null" {
		fe.Add("seed", "The seed has to be a JSON array. Use [] for an empty collection.")
		return
	}

	var elements []json.RawMessage
	if err := json.Unmarshal([]byte(seed), &elements); err != nil {
		if json.Valid([]byte(seed)) {
			fe.Add("seed", "The seed has to be a JSON array, even when it holds one document.")
			return
		}
		fe.Add("seed", "That is not valid JSON: "+err.Error())
		return
	}
	if len(elements) > maxSeedElements {
		fe.Add("seed", fmt.Sprintf("Use %d documents or fewer.", maxSeedElements))
		return
	}

	for i, element := range elements {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(element, &object); err != nil {
			fe.Add("seed", fmt.Sprintf("Entry %d is not a JSON object. Every document in a collection is an object.", i+1))
			return
		}
	}
}

// Limits on an API token definition.
const (
	maxTokenNameLen = 80
	// maxTokenDays bounds the expiry field. Ten years is not a security
	// property; it is a bound on what a mistyped number can produce.
	maxTokenDays = 3650
)

func validateTokenName(fe *FieldErrors, name string) {
	switch {
	case name == "":
		fe.Add("name", "Name the token — it is how you will know which one to revoke.")
	case utf8.RuneCountInString(name) > maxTokenNameLen:
		fe.Add("name", fmt.Sprintf("Use %d characters or fewer.", maxTokenNameLen))
	case strings.ContainsFunc(name, isControl):
		fe.Add("name", "Remove the control characters.")
	}
}

func validateTokenExpiry(fe *FieldErrors, days int) {
	if days < 0 || days > maxTokenDays {
		fe.Add("expires_in_days",
			fmt.Sprintf("Use a number of days between 1 and %d, or 0 for a token that does not expire.", maxTokenDays))
	}
}

// canonicalHeaderName is the form a header is stored and compared in.
func canonicalHeaderName(name string) string {
	return textproto.CanonicalMIMEHeaderKey(name)
}

// validHeaderName reports whether name is an RFC 9110 field name — a token, and
// nothing else. textproto leaves a name it cannot canonicalise untouched, so
// this is what catches it.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return !strings.ContainsRune(
			"!#$%&'*+-.^_`|~0123456789"+
				"abcdefghijklmnopqrstuvwxyz"+
				"ABCDEFGHIJKLMNOPQRSTUVWXYZ", r)
	})
}
