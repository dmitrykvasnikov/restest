package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The conditions callers are expected to recognise and act on. Everything else
// coming out of this package is an unexpected failure and belongs in the log,
// not in a form.
var (
	// ErrNotFound is returned when a row does not exist, or exists but belongs
	// to somebody else. The two are deliberately the same answer: telling a
	// caller that a project exists but is not theirs is telling them something
	// about another account.
	ErrNotFound = errors.New("not found")

	// ErrInvalidCredentials covers both an unknown address and a wrong
	// password, so that a login form cannot be used to discover who has an
	// account here.
	ErrInvalidCredentials = errors.New("invalid credentials")

	ErrEmailTaken = errors.New("email is already registered")
	ErrSlugTaken  = errors.New("slug is already taken")
)

// FieldErrors reports which form fields were rejected and why. Validation lives
// here rather than in the HTTP layer so that the management API in M6 and the
// phase 2 runner enforce exactly the same rules as the browser forms.
//
// The zero value is unusable; build one with Add, which allocates as needed.
type FieldErrors map[string]string

// Error renders every field message in a stable order, so a log line or a test
// failure reads the same way twice.
func (e FieldErrors) Error() string {
	fields := make([]string, 0, len(e))
	for f := range e {
		fields = append(fields, f)
	}
	slices.Sort(fields)

	var b strings.Builder
	b.WriteString("invalid input: ")
	for i, f := range fields {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", f, e[f])
	}
	return b.String()
}

// Add records a message against a field, keeping the first one: the earliest
// check is the most specific, and a form field with two complaints on it helps
// nobody.
func (e *FieldErrors) Add(field, message string) {
	if *e == nil {
		*e = make(FieldErrors)
	}
	if _, exists := (*e)[field]; !exists {
		(*e)[field] = message
	}
}

// orNil returns the errors as an error, or a nil error when there are none, so
// that callers can `return v, fe.orNil()` without a length check.
func (e FieldErrors) orNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}
