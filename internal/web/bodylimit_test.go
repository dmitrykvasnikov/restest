package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The cap is applied once, above every handler, because the handlers that
// forget to apply one of their own are exactly the handlers where it would
// matter.
func TestTheBodyCapCoversEveryHandler(t *testing.T) {
	const cap = 4096
	tweak := func(o *Options) { o.MaxRequestBody = cap }

	t.Run("mock writes", func(t *testing.T) {
		b, docs := withCollectionOptions(t, core.Headers{}, tweak)

		huge := `{"note":"` + strings.Repeat("x", cap) + `"}`
		resp, body := b.do(http.MethodPost, mockPath(""), huge)

		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", resp.StatusCode, body)
		}
		// The refusal names the cap that was actually applied, not a constant
		// somewhere that has drifted from it.
		if !strings.Contains(body, "4096") {
			t.Errorf("the refusal does not name the configured cap: %s", body)
		}
		if n := docs.count(); n != 0 {
			t.Errorf("%d documents were stored from a refused write, want 0", n)
		}
	})

	t.Run("the management API", func(t *testing.T) {
		b := newBrowserWith(t, apiStore(), tweak)
		logIn(t, b)
		s := b.script(mintToken(t, b, "ci"))

		huge := `{"slug":"big","name":"` + strings.Repeat("x", cap) + `"}`
		resp, body := s.do(http.MethodPost, "/api/v1/projects", huge)

		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "larger than") {
			t.Errorf("the refusal does not say what happened: %s", body)
		}
	})

	t.Run("a browser form", func(t *testing.T) {
		b := newBrowserWith(t, projectStore(), tweak)
		logIn(t, b)

		resp, _ := b.post("/projects/new", "/projects", url.Values{
			"slug": {"checkout"},
			"name": {strings.Repeat("x", cap)},
		})
		// However it is reported, the one thing that must not happen is a 2xx:
		// that would mean the whole body was read and acted on.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.Errorf("status = %d, want the oversized form to be refused", resp.StatusCode)
		}
	})
}

// The cap defaults to the same 1 MiB the mock server has always applied, so a
// Server built by hand behaves like the one main.go builds.
func TestTheBodyCapHasADefault(t *testing.T) {
	srv := newServer(t, stubStore{})
	if srv.maxRequestBody != defaultMaxRequestBody {
		t.Errorf("maxRequestBody = %d, want %d", srv.maxRequestBody, defaultMaxRequestBody)
	}
}
