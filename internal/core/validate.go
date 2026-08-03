package core

import (
	"net/mail"
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
