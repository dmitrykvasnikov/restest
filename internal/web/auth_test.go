package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// accountStore is the stub for the ordinary case: one account that registers,
// logs in and is found again on every later request.
func accountStore() stubStore {
	return stubStore{
		registerUser: func(_ context.Context, email, _ string) (core.User, error) {
			return core.User{ID: testUser.ID, Email: email}, nil
		},
		authenticate: func(_ context.Context, email, _ string) (core.User, error) {
			return core.User{ID: testUser.ID, Email: email}, nil
		},
		userByID: func(_ context.Context, id uuid.UUID) (core.User, error) {
			if id != testUser.ID {
				return core.User{}, core.ErrNotFound
			}
			return testUser, nil
		},
	}
}

// logIn drives the real login form, so every test that needs an account starts
// from a session built the way a browser builds one.
func logIn(t *testing.T, b *browser) {
	t.Helper()

	resp, body := b.post("/login", "/login", url.Values{
		"email":    {testUser.Email},
		"password": {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("log in: status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
}

func TestRegisterCreatesAccountAndLogsIn(t *testing.T) {
	var gotEmail, gotPassword string
	store := accountStore()
	store.registerUser = func(_ context.Context, email, password string) (core.User, error) {
		gotEmail, gotPassword = email, password
		return core.User{ID: testUser.ID, Email: email}, nil
	}

	b := newBrowser(t, store)
	resp, body := b.post("/register", "/register", url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if gotEmail != "sam@example.com" || gotPassword != "correct horse battery staple" {
		t.Errorf("store saw email %q and password %q", gotEmail, gotPassword)
	}
	// Registering ends on the project list, logged in.
	if !strings.Contains(body, "Projects") {
		t.Errorf("did not land on the project list:\n%s", body)
	}
	if !strings.Contains(body, "Log out") {
		t.Error("the page does not show a logged-in navigation")
	}
	// The welcome flash is shown once and then gone.
	if !strings.Contains(body, "Create your first project") {
		t.Error("no welcome flash on the first page")
	}
	if _, again := b.get("/projects"); strings.Contains(again, "Welcome.") {
		t.Error("the flash was shown a second time")
	}
}

func TestRegisterRejectsInvalidInput(t *testing.T) {
	store := accountStore()
	store.registerUser = func(context.Context, string, string) (core.User, error) {
		return core.User{}, core.FieldErrors{"password": "Use at least 8 characters."}
	}

	b := newBrowser(t, store)
	resp, body := b.post("/register", "/register", url.Values{
		"email":    {"sam@example.com"},
		"password": {"short"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Use at least 8 characters.") {
		t.Errorf("the field message is not on the page:\n%s", body)
	}
	// The address comes back filled in; the password never does.
	if !strings.Contains(body, `value="sam@example.com"`) {
		t.Error("the email was not preserved for a second attempt")
	}
	if strings.Contains(body, "short") {
		t.Error("the rejected password was written back into the page")
	}
}

func TestRegisterReportsTakenEmail(t *testing.T) {
	store := accountStore()
	store.registerUser = func(context.Context, string, string) (core.User, error) {
		return core.User{}, core.ErrEmailTaken
	}

	b := newBrowser(t, store)
	resp, body := b.post("/register", "/register", url.Values{
		"email":    {"sam@example.com"},
		"password": {"correct horse battery staple"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "already registered") {
		t.Errorf("the page does not say the address is taken:\n%s", body)
	}
}

// Wrong address and wrong password get the same answer, so the form cannot be
// used to find out who has an account here.
func TestLoginRejectsBadCredentials(t *testing.T) {
	store := accountStore()
	store.authenticate = func(context.Context, string, string) (core.User, error) {
		return core.User{}, core.ErrInvalidCredentials
	}

	b := newBrowser(t, store)
	resp, body := b.post("/login", "/login", url.Values{
		"email":    {"nobody@example.com"},
		"password": {"wrong"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "do not match an account") {
		t.Errorf("no rejection message on the page:\n%s", body)
	}
	if strings.Contains(body, "Log out") {
		t.Error("a failed login produced a logged-in page")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	for _, tt := range []struct {
		name       string
		secure     bool
		wantSecure bool
	}{
		{"plain http development", false, false},
		{"behind TLS", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := newBrowserWith(t, accountStore(), func(o *Options) {
				o.Sessions = newSessionManager(tt.secure)
			}).noFollow()

			// The CSRF cookie is issued on the first page load, the session
			// cookie once the login writes to the session.
			formResp, _ := b.get("/login")
			loginResp, _ := b.post("/login", "/login", url.Values{
				"email":    {testUser.Email},
				"password": {"correct horse battery staple"},
			})

			cookies := map[string]*http.Cookie{
				"csrf_token":      setCookie(t, formResp, "csrf_token"),
				"restest_session": setCookie(t, loginResp, "restest_session"),
			}
			for name, cookie := range cookies {
				if !cookie.HttpOnly {
					t.Errorf("%s is readable from JavaScript", name)
				}
				if cookie.SameSite != http.SameSiteLaxMode {
					t.Errorf("%s SameSite = %v, want Lax", name, cookie.SameSite)
				}
				if cookie.Secure != tt.wantSecure {
					t.Errorf("%s Secure = %v, want %v", name, cookie.Secure, tt.wantSecure)
				}
			}
		})
	}
}

// Reusing the pre-login token would leave a session fixed by an attacker before
// login still valid after it.
func TestLoginRenewsTheSessionToken(t *testing.T) {
	b := newBrowser(t, accountStore()).noFollow()

	// Asking for a private page while anonymous writes to the session — the
	// destination to return to — so a token exists before the login does.
	resp, _ := b.get("/projects")
	before := setCookie(t, resp, "restest_session").Value

	resp, _ = b.post("/login", "/login", url.Values{
		"email":    {testUser.Email},
		"password": {"correct horse battery staple"},
	})

	after := setCookie(t, resp, "restest_session").Value
	if after == before {
		t.Error("the session token survived the login")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	b := newBrowser(t, accountStore())
	logIn(t, b)

	resp, body := b.post("/projects", "/logout", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect", resp.StatusCode)
	}
	if !strings.Contains(body, "You are logged out.") {
		t.Errorf("no confirmation of the logout:\n%s", body)
	}

	// The account is gone from the session, so a private page sends us back.
	_, after := b.get("/projects")
	if strings.Contains(after, "Log out") {
		t.Error("still logged in after logging out")
	}
}

// A session naming a user who no longer exists is thrown away rather than
// carried around, so a deleted account cannot keep browsing.
func TestStaleSessionIsDiscarded(t *testing.T) {
	var deleted atomic.Bool

	store := accountStore()
	store.userByID = func(context.Context, uuid.UUID) (core.User, error) {
		if deleted.Load() {
			return core.User{}, core.ErrNotFound
		}
		return testUser, nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	deleted.Store(true)

	_, body := b.get("/projects")
	if strings.Contains(body, "Log out") {
		t.Errorf("a session for a deleted account still browses:\n%s", body)
	}
}

func TestAnonymousIsSentToLoginAndBack(t *testing.T) {
	store := accountStore()
	store.projectByOwnerAndSlug = func(_ context.Context, _ uuid.UUID, slug string) (core.Project, error) {
		return core.Project{ID: testProjectID, Slug: slug, Name: "Checkout"}, nil
	}

	b := newBrowser(t, store).noFollow()

	resp, _ := b.get("/projects/checkout")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Fatalf("Location = %q, want /login", got)
	}

	// Logging in continues to where the user was going, not to a generic page.
	b.client.CheckRedirect = nil
	resp, _ = b.post("/login", "/login", url.Values{
		"email":    {testUser.Email},
		"password": {"correct horse battery staple"},
	})
	if got := resp.Request.URL.Path; got != "/projects/checkout" {
		t.Errorf("landed on %q, want the page originally asked for", got)
	}
}

// setCookie reads a cookie out of a response's Set-Cookie headers, which unlike
// the jar preserve the attributes the test is asking about.
func setCookie(t *testing.T, resp *http.Response, name string) *http.Cookie {
	t.Helper()

	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("response set no cookie named %q (got %v)", name, resp.Cookies())
	return nil
}
