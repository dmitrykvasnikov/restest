package core

import (
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"sam@example.com", "sam@example.com"},
		{"  sam@example.com\t", "sam@example.com"},
		{"SAM@Example.COM", "sam@example.com"},
	}
	for _, tt := range tests {
		if got := NormalizeEmail(tt.in); got != tt.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		name  string
		email string
		valid bool
	}{
		{"ordinary", "sam@example.com", true},
		{"subdomain", "sam@mail.example.co.uk", true},
		{"plus addressing", "sam+restest@example.com", true},
		{"empty", "", false},
		{"no at sign", "sam.example.com", false},
		{"two at signs", "sam@@example.com", false},
		{"no domain", "sam@", false},
		{"display name form", "Sam <sam@example.com>", false},
		{"trailing comment", "sam@example.com (Sam)", false},
		{"too long", strings.Repeat("a", maxEmailLen) + "@example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fe FieldErrors
			validateEmail(&fe, tt.email)

			if got := len(fe) == 0; got != tt.valid {
				t.Errorf("validateEmail(%q) accepted = %v, want %v (%v)", tt.email, got, tt.valid, fe)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		valid    bool
	}{
		{"at the minimum", "12345678", true},
		{"a passphrase", "correct horse battery staple", true},
		{"eight runes, not eight bytes", "паролька", true},
		{"empty", "", false},
		{"one short", "1234567", false},
		{"absurdly long", strings.Repeat("x", maxPasswordLen+1), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fe FieldErrors
			validatePassword(&fe, tt.password)

			if got := len(fe) == 0; got != tt.valid {
				t.Errorf("accepted = %v, want %v (%v)", got, tt.valid, fe)
			}
		})
	}
}

// The slug rule has to agree with the projects_slug_format check constraint in
// migration 00001. A value that passes here and fails there would surface as a
// 500 rather than as a message beside the field.
func TestValidateSlug(t *testing.T) {
	tests := []struct {
		name  string
		slug  string
		valid bool
	}{
		{"single character", "a", true},
		{"single digit", "7", true},
		{"hyphenated", "my-mock-api", true},
		{"digits and letters", "api2v3", true},
		{"at the length limit", strings.Repeat("a", maxSlugLen), true},

		{"empty", "", false},
		{"one over the limit", strings.Repeat("a", maxSlugLen+1), false},
		{"upper case", "MyAPI", false},
		{"leading hyphen", "-mock", false},
		{"trailing hyphen", "mock-", false},
		{"underscore", "my_mock", false},
		{"space", "my mock", false},
		{"dot", "my.mock", false},
		{"slash", "my/mock", false},
		{"non-ascii", "макет", false},

		// Held back for the application's own paths and for the shared demo
		// project, so that subdomain addressing stays possible later.
		{"reserved: api", "api", false},
		{"reserved: admin", "admin", false},
		{"reserved: demo", "demo", false},
		{"reserved: healthz", "healthz", false},
		{"reserved: m", "m", false},
		{"reserved: static", "static", false},
		{"not reserved: apis", "apis", true},
		{"not reserved: my-demo", "my-demo", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fe FieldErrors
			validateSlug(&fe, tt.slug)

			if got := len(fe) == 0; got != tt.valid {
				t.Errorf("validateSlug(%q) accepted = %v, want %v (%v)", tt.slug, got, tt.valid, fe)
			}
		})
	}
}

func TestValidateProjectName(t *testing.T) {
	tests := []struct {
		name    string
		project string
		valid   bool
	}{
		{"ordinary", "Checkout API", true},
		{"non-ascii", "Макет платежей", true},
		{"at the length limit", strings.Repeat("n", maxNameLen), true},
		{"empty", "", false},
		{"one over the limit", strings.Repeat("n", maxNameLen+1), false},
		{"newline", "two\nlines", false},
		{"terminal escape", "colour\x1b[31m", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fe FieldErrors
			validateProjectName(&fe, tt.project)

			if got := len(fe) == 0; got != tt.valid {
				t.Errorf("accepted = %v, want %v (%v)", got, tt.valid, fe)
			}
		})
	}
}

// Add keeps the first message for a field: the earliest check is the most
// specific, and two complaints about one field help nobody.
func TestFieldErrorsAddKeepsTheFirst(t *testing.T) {
	var fe FieldErrors
	fe.Add("slug", "first")
	fe.Add("slug", "second")

	if fe["slug"] != "first" {
		t.Errorf("slug = %q, want the first message", fe["slug"])
	}
}

func TestFieldErrorsMessageIsOrdered(t *testing.T) {
	fe := FieldErrors{"slug": "bad slug", "email": "bad email", "name": "bad name"}

	const want = "invalid input: email: bad email; name: bad name; slug: bad slug"
	if got := fe.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
