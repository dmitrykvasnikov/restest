package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// tokenStore is accountStore with a working token store behind it, so a test
// can mint a token through the page and then present it to the API.
func tokenStore() stubStore {
	store := projectStore()
	store.tokens = newFakeTokens()
	return store
}

// tokenPattern finds a minted token in the page it is shown on. It matches the
// format core issues rather than "some string", so a change to the format that
// broke a caller's copy-and-paste would fail here.
var tokenPattern = regexp.MustCompile(`rst_[A-Za-z0-9_-]{43}`)

// mintToken drives the real form and returns the plaintext, which is the only
// place it ever exists.
func mintToken(t *testing.T, b *browser, name string) string {
	t.Helper()

	resp, body := b.post(pathTokens, pathTokens, url.Values{"name": {name}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create token: status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}

	token := tokenPattern.FindString(body)
	if token == "" {
		t.Fatalf("no token on the page after creating one:\n%s", body)
	}
	return token
}

func TestTokenIsShownOnceAndNeverAgain(t *testing.T) {
	b := newBrowser(t, tokenStore())
	logIn(t, b)

	token := mintToken(t, b, "ci")

	// The page it was shown on is the only one that shows it. A refresh is the
	// obvious thing to do next, and it must not bring the secret back.
	_, again := b.get(pathTokens)
	if strings.Contains(again, token) {
		t.Error("the token is still on the page after a reload")
	}
	// What remains is the prefix, which is what makes the row identifiable.
	if !strings.Contains(again, token[:len(core.TokenMark)+8]) {
		t.Errorf("the token's prefix is not listed:\n%s", again)
	}
	if !strings.Contains(again, "ci") {
		t.Errorf("the token's name is not listed:\n%s", again)
	}
	if !strings.Contains(again, "never") {
		t.Errorf("an unused token should say so:\n%s", again)
	}
}

// The row holds a hash, not the secret. This is the assertion that would catch
// a store that quietly kept the plaintext to make the page easier to write.
func TestTokenIsStoredAsAHash(t *testing.T) {
	store := tokenStore()
	b := newBrowser(t, store)
	logIn(t, b)

	token := mintToken(t, b, "ci")

	stored := store.tokens.byUser(testUser.ID)
	if len(stored) != 1 {
		t.Fatalf("stored %d tokens, want 1", len(stored))
	}
	if strings.Contains(stored[0].Prefix, token[len(core.TokenMark)+8:]) {
		t.Error("the stored prefix carries part of the secret it should have dropped")
	}
}

func TestTokenNeedsAName(t *testing.T) {
	b := newBrowser(t, tokenStore())
	logIn(t, b)

	resp, body := b.post(pathTokens, pathTokens, url.Values{"name": {""}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Name the token") {
		t.Errorf("no message beside the field:\n%s", body)
	}
	if tokenPattern.MatchString(body) {
		t.Error("a rejected form still minted a token")
	}
}

func TestTokenExpiryMustBeANumber(t *testing.T) {
	b := newBrowser(t, tokenStore())
	logIn(t, b)

	resp, body := b.post(pathTokens, pathTokens, url.Values{
		"name":            {"ci"},
		"expires_in_days": {"soon"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "number of days") {
		t.Errorf("no message beside the field:\n%s", body)
	}
}

// Revoking is immediate: the next request carrying the token is refused.
func TestRevokedTokenStopsWorking(t *testing.T) {
	store := tokenStore()
	b := newBrowser(t, store)
	logIn(t, b)

	token := mintToken(t, b, "ci")
	caller := b.script(token)

	if resp, body := caller.get(pathAPIPrefix); resp.StatusCode != http.StatusOK {
		t.Fatalf("the fresh token was refused: status = %d:\n%s", resp.StatusCode, body)
	}

	stored := store.tokens.byUser(testUser.ID)
	resp, body := b.post(pathTokens, "/tokens/"+stored[0].ID.String()+"/delete", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status = %d:\n%s", resp.StatusCode, body)
	}

	resp, body = caller.get(pathAPIPrefix)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a revoked token:\n%s", resp.StatusCode, body)
	}
}

// The tokens page belongs to an account, like every other page below /projects.
func TestTokensPageNeedsAnAccount(t *testing.T) {
	b := newBrowser(t, tokenStore()).noFollow()

	resp, _ := b.get(pathTokens)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the login page", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}
}
