//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// --- the store half ---------------------------------------------------------

func TestAPITokenRoundTrip(t *testing.T) {
	store := newStore(t)

	user, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	token, plaintext, err := store.CreateAPIToken(t.Context(), user.ID, core.APITokenInput{Name: "ci"})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if !strings.HasPrefix(plaintext, token.Prefix) {
		t.Errorf("prefix %q does not identify the token %q", token.Prefix, plaintext)
	}
	if token.Used() {
		t.Error("a token that has authenticated nothing reports a last use")
	}
	if token.Expires() {
		t.Error("no expiry was asked for and one was set")
	}

	// The row holds the hash and nothing that can produce the secret.
	var storedHash []byte
	if err := store.Pool().QueryRow(t.Context(),
		`select token_hash from api_tokens where id = $1`, token.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read the stored hash: %v", err)
	}
	if string(storedHash) != string(core.HashToken(plaintext)) {
		t.Error("the stored hash is not the hash of the token that was issued")
	}
	if strings.Contains(string(storedHash), plaintext) {
		t.Error("the plaintext is in the row")
	}

	// It authenticates its owner, and says so in the row.
	authenticated, used, err := store.AuthenticateAPIToken(t.Context(), plaintext)
	if err != nil {
		t.Fatalf("AuthenticateAPIToken: %v", err)
	}
	if authenticated.ID != user.ID || authenticated.Email != user.Email {
		t.Errorf("authenticated %+v, want %s", authenticated, user.Email)
	}
	if !used.Used() {
		t.Error("last_used_at was not recorded")
	}
	if time.Since(used.LastUsedAt) > time.Minute {
		t.Errorf("last_used_at = %v, want now", used.LastUsedAt)
	}

	// And the listing shows it without ever holding the secret.
	tokens, err := store.APITokensByUser(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("APITokensByUser: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Name != "ci" || !tokens[0].Used() {
		t.Errorf("listed %+v, want one used token named ci", tokens)
	}
}

func TestRevokedTokenAuthenticatesNothing(t *testing.T) {
	store := newStore(t)

	user, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	token, plaintext, err := store.CreateAPIToken(t.Context(), user.ID, core.APITokenInput{Name: "ci"})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if err := store.DeleteAPIToken(t.Context(), user.ID, token.ID); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}
	if _, _, err := store.AuthenticateAPIToken(t.Context(), plaintext); !errors.Is(err, core.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken after revocation", err)
	}

	// Revoking somebody else's is the same answer as revoking one that never
	// existed: nothing to tell them apart by.
	if err := store.DeleteAPIToken(t.Context(), uuid.New(), token.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Expiry is enforced in the statement, not in Go, so it is enforced whichever
// caller asks — and an expired token does not get its last_used_at bumped by a
// request it did not authorise.
func TestExpiredTokenIsRefused(t *testing.T) {
	store := newStore(t)

	user, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	token, plaintext, err := store.CreateAPIToken(t.Context(), user.ID, core.APITokenInput{
		Name:          "temporary",
		ExpiresInDays: 1,
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if !token.Expires() {
		t.Fatal("an expiry was asked for and none was set")
	}

	// Move it into the past rather than waiting a day for it.
	if _, err := store.Pool().Exec(t.Context(),
		`update api_tokens set expires_at = now() - interval '1 second' where id = $1`, token.ID); err != nil {
		t.Fatalf("age the token: %v", err)
	}

	if _, _, err := store.AuthenticateAPIToken(t.Context(), plaintext); !errors.Is(err, core.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken for an expired token", err)
	}

	var lastUsed *time.Time
	if err := store.Pool().QueryRow(t.Context(),
		`select last_used_at from api_tokens where id = $1`, token.ID).Scan(&lastUsed); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if lastUsed != nil {
		t.Errorf("last_used_at = %v, want null: the request was refused", *lastUsed)
	}
}

// --- the HTTP half ----------------------------------------------------------

// scripted is a client with a token and no cookie jar — what a CI job is. It
// exists to make the absence of a session unmistakable: nothing it does can be
// explained by a cookie it does not have.
type scripted struct {
	t     *testing.T
	site  *site
	token string
}

func (s *site) scripted(token string) *scripted {
	return &scripted{t: s.t, site: s, token: token}
}

func (c *scripted) do(method, path, body string) (*http.Response, string) {
	c.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, c.site.url+path, reader)
	if err != nil {
		c.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	// A client of its own: no jar, so no session can be reused by accident.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp, readBody(c.t, resp)
}

// expect runs a call and fails the test unless the status is the one wanted,
// which keeps the milestone test readable as the script it stands for.
func (c *scripted) expect(status int, method, path, body string) string {
	c.t.Helper()

	resp, out := c.do(method, path, body)
	if resp.StatusCode != status {
		c.t.Fatalf("%s %s: status = %d, want %d\n%s", method, path, resp.StatusCode, status, out)
	}
	return out
}

var tokenPattern = regexp.MustCompile(`rst_[A-Za-z0-9_-]{43}`)

// registerAndMint takes an account from nothing to a working token the way a
// person does: register, open the tokens page, submit the form, copy what it
// showed.
func registerAndMint(t *testing.T, s *site, email, name string) string {
	t.Helper()

	if resp, body := s.submit("/register", "/register", formValues(map[string]string{
		"email": email, "password": testPassword,
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d\n%s", resp.StatusCode, body)
	}

	resp, body := s.submit("/tokens", "/tokens", url.Values{"name": {name}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create token: status = %d\n%s", resp.StatusCode, body)
	}

	token := tokenPattern.FindString(body)
	if token == "" {
		t.Fatalf("no token on the page after creating one:\n%s", body)
	}
	return token
}

// TestTheM6Milestone is the milestone's "done when" driven end to end: a shell
// script can create a project, define endpoints and reset state.
//
// Everything after the token is minted runs over /api/v1/ with a bearer token
// and no cookie, against real Postgres and the real matcher — and the mock URLs
// it configures answer immediately, which is the part that makes it worth
// scripting at all.
func TestTheM6Milestone(t *testing.T) {
	s := newSite(t)
	token := registerAndMint(t, s, "sam@example.com", "ci")
	ci := s.scripted(token)

	// --- the token is good, and says whose it is --------------------------
	var index struct {
		User            string   `json:"user"`
		AuthenticatedBy string   `json:"authenticated_by"`
		Routes          []string `json:"routes"`
	}
	decodeInto(t, ci.expect(http.StatusOK, http.MethodGet, "/api/v1/", ""), &index)
	if index.User != "sam@example.com" || index.AuthenticatedBy != "token" {
		t.Errorf("index = %+v, want the account the token belongs to", index)
	}

	// --- create a project -------------------------------------------------
	ci.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects",
		`{"slug":"ci","name":"CI fixtures"}`)

	// --- define a static endpoint and a collection behind another ---------
	ci.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects/ci/collections",
		`{"name":"users","seed":[{"id":1,"name":"Ada"}]}`)
	ci.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects/ci/endpoints",
		`{"method":"GET","path":"/health","status_code":200,"body":"{\"ok\":true}",`+
			`"headers":{"Content-Type":"application/json"}}`)
	// No "kind": naming a collection can mean nothing else.
	ci.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects/ci/endpoints",
		`{"path":"/users","collection":"users"}`)

	// --- and they answer at once, with no restart and no deploy -----------
	resp, body := s.get("/m/ci/health")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("GET /m/ci/health: status = %d\n%s", resp.StatusCode, body)
	}
	if got := len(decodeArray(t, mustGet(t, s, "/m/ci/users"))); got != 1 {
		t.Fatalf("the collection returned %d documents, want the 1 the seed named", got)
	}

	// --- a client writes to the mock, as a test run would -----------------
	if resp, body := s.send(http.MethodPost, "/m/ci/users", `{"name":"Grace"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST a document: status = %d\n%s", resp.StatusCode, body)
	}
	if got := len(decodeArray(t, mustGet(t, s, "/m/ci/users"))); got != 2 {
		t.Fatalf("the write did not stick: %d documents, want 2", got)
	}

	// --- reset, which is what a suite does between runs -------------------
	var reset struct {
		Project    string `json:"project"`
		Collection string `json:"collection"`
		Documents  int    `json:"documents"`
	}
	decodeInto(t, ci.expect(http.StatusOK, http.MethodPost,
		"/api/v1/projects/ci/collections/users/reset", ""), &reset)
	if reset.Documents != 1 {
		t.Errorf("reset left %d documents, want the 1 in the seed", reset.Documents)
	}

	documents := decodeArray(t, mustGet(t, s, "/m/ci/users"))
	if len(documents) != 1 {
		t.Fatalf("after the reset there are %d documents, want 1", len(documents))
	}
	if name, _ := documents[0]["name"].(string); name != "Ada" {
		t.Errorf("the surviving document is %v, want the seeded Ada", documents[0])
	}

	// --- the log is readable from the script too --------------------------
	var log struct {
		Count int `json:"count"`
		Items []struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Path   string `json:"path"`
			Status int    `json:"status"`
		} `json:"items"`
	}
	// The write is off the request path by design, so a test that looked
	// immediately would be testing the timing rather than the behaviour.
	waitForExchanges(t, s, 4)
	decodeInto(t, ci.expect(http.StatusOK, http.MethodGet, "/api/v1/projects/ci/log", ""), &log)
	if log.Count < 4 {
		t.Fatalf("the log holds %d entries, want the mock requests that were sent", log.Count)
	}

	var detail struct {
		Request struct {
			Text *string `json:"text"`
		} `json:"request_body"`
	}
	for _, entry := range log.Items {
		if entry.Method != http.MethodPost {
			continue
		}
		decodeInto(t, ci.expect(http.StatusOK, http.MethodGet,
			"/api/v1/projects/ci/log/"+entry.ID, ""), &detail)
		if detail.Request.Text == nil || !strings.Contains(*detail.Request.Text, "Grace") {
			t.Errorf("the recorded request body is %v, want the document that was posted", detail.Request.Text)
		}
		break
	}

	// --- and the token's use was written down -----------------------------
	var lastUsed *time.Time
	if err := s.pool.QueryRow(t.Context(),
		`select last_used_at from api_tokens where name = 'ci'`).Scan(&lastUsed); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if lastUsed == nil {
		t.Error("the token authenticated a dozen requests and reports never having been used")
	}

	// --- tearing down is scriptable as well -------------------------------
	ci.expect(http.StatusNoContent, http.MethodDelete, "/api/v1/projects/ci", "")
	if resp, _ := s.get("/m/ci/users"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the deleted project still answers: status = %d", resp.StatusCode)
	}
}

// A token reaches its own account's projects and nothing else. The ownership
// check is the same SQL the interface goes through, which is the point: there
// is no second path for a script to slip down.
func TestATokenSeesOnlyItsOwnAccount(t *testing.T) {
	s := newSite(t)

	token := registerAndMint(t, s, "sam@example.com", "sam's ci")
	sam := s.scripted(token)
	sam.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects", `{"slug":"sams","name":"Sam"}`)

	// A second account, in its own browser, with its own token.
	other := newSiteSharing(t, s)
	intruder := other.scripted(registerAndMint(t, other, "kim@example.com", "kim's ci"))

	// The same 404 a slug nobody has ever used would get.
	intruder.expect(http.StatusNotFound, http.MethodGet, "/api/v1/projects/sams", "")
	intruder.expect(http.StatusNotFound, http.MethodPost, "/api/v1/projects/sams/collections",
		`{"name":"users"}`)

	var list struct {
		Count int `json:"count"`
	}
	decodeInto(t, intruder.expect(http.StatusOK, http.MethodGet, "/api/v1/projects", ""), &list)
	if list.Count != 0 {
		t.Errorf("the other account lists %d projects, want none of Sam's", list.Count)
	}

	// And the mock traffic is unaffected either way: it needs no token and
	// takes none.
	sam.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects/sams/collections",
		`{"name":"users","seed":[{"id":1}]}`)
	sam.expect(http.StatusCreated, http.MethodPost, "/api/v1/projects/sams/endpoints",
		`{"path":"/users","collection":"users"}`)
	if resp, body := s.get("/m/sams/users"); resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous mock traffic was refused: status = %d\n%s", resp.StatusCode, body)
	}
}

// mustGet is s.get for the calls whose failure would make the rest of a test
// meaningless.
func mustGet(t *testing.T, s *site, path string) string {
	t.Helper()

	resp, body := s.get(path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d\n%s", path, resp.StatusCode, body)
	}
	return body
}

func decodeInto(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}
