package web

import (
	"bufio"
	"context"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// listEndpoint is what the inspector tests send at to produce something to
// inspect.
var listEndpoint = mockEndpoint{
	method: "GET", path: "/orders", status: 200, body: `[{"id":1}]`,
}

func TestLogListShowsWhatWasServed(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, listEndpoint)

	b.get("/m/checkout/orders?status=paid")
	logIn(t, b)

	resp, body := b.get("/projects/checkout/log")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "/orders?status=paid") {
		t.Errorf("the request is not on the page:\n%s", body)
	}
	if !strings.Contains(body, "GET") {
		t.Error("the method is not shown")
	}
	if !strings.Contains(body, ">200<") {
		t.Error("the status code is not shown")
	}
}

func TestLogListIsEmptyForAQuietProject(t *testing.T) {
	b := logBrowser(t, newFakeLog(), defaultLogBodyLimit, listEndpoint)
	logIn(t, b)

	_, body := b.get("/projects/checkout/log")
	if !strings.Contains(body, "Nothing recorded yet") {
		t.Errorf("no empty state on the log page:\n%s", body)
	}
}

// A gap in the log has to be visible as a gap. This is the whole reason the
// recorder counts drops instead of discarding quietly.
func TestLogListReportsDrops(t *testing.T) {
	log := newFakeLog()
	log.full = true

	b := logBrowser(t, log, defaultLogBodyLimit, listEndpoint)
	b.get("/m/checkout/orders")
	logIn(t, b)

	_, body := b.get("/projects/checkout/log")
	if !strings.Contains(body, "were not recorded") {
		t.Errorf("the page does not admit to the dropped exchange:\n%s", body)
	}
	if !strings.Contains(body, "Nothing recorded yet") {
		t.Error("the dropped exchange appears to have been recorded after all")
	}
}

// Somebody else's project is the same 404 as one that does not exist, on this
// page as everywhere else.
func TestLogListRefusesAnotherAccountsProject(t *testing.T) {
	b := logBrowser(t, newFakeLog(), defaultLogBodyLimit, listEndpoint)
	logIn(t, b)

	resp, body := b.get("/projects/somebodyelse/log")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No such project") {
		t.Errorf("unexpected page:\n%s", body)
	}
}

func TestLogListNeedsAnAccount(t *testing.T) {
	b := logBrowser(t, newFakeLog(), defaultLogBodyLimit, listEndpoint).noFollow()

	resp, _ := b.get("/projects/checkout/log")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the login page", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}
}

func TestLogEntryShowsHeadersAndBodies(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, echoEndpoint)

	b.do(http.MethodPost, "/m/checkout/orders", `{"sku":"abc"}`)
	logIn(t, b)

	ex, ok := log.last()
	if !ok {
		t.Fatal("nothing was recorded")
	}

	resp, body := b.get("/projects/checkout/log/" + ex.ID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "&#34;sku&#34;: &#34;abc&#34;") {
		t.Errorf("the request body is not shown, or was not reformatted:\n%s", body)
	}
	if !strings.Contains(body, "X-Mock") {
		t.Error("the response headers are not shown")
	}
	if !strings.Contains(body, "201") {
		t.Error("the status code is not shown")
	}
}

// An exchange that has aged out of the log, and one that never existed, are the
// same answer.
func TestLogEntryNotFound(t *testing.T) {
	b := logBrowser(t, newFakeLog(), defaultLogBodyLimit, listEndpoint)
	logIn(t, b)

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		resp, body := b.get("/projects/checkout/log/" + id)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status for %q = %d, want 404", id, resp.StatusCode)
		}
		if !strings.Contains(body, "No such exchange") {
			t.Errorf("unexpected page for %q:\n%s", id, body)
		}
	}
}

// The one M4 is really about: a request that arrives after the page was
// rendered reaches the browser without a refresh.
func TestLogStreamSendsWhatArrivesNext(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, listEndpoint)
	logIn(t, b)

	// One exchange before the stream opens, so that the cursor the stream
	// starts from is a real one and the test can tell "everything since" from
	// "everything".
	b.get("/m/checkout/orders?first=1")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+"/projects/checkout/log/stream", nil)
	if err != nil {
		t.Fatalf("build the stream request: %v", err)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test client response

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	// The stream is open before the request it is meant to deliver is made,
	// which is the sequence a user sees: page open, request sent, row appears.
	b.get("/m/checkout/orders?second=2")

	var event strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		event.WriteString(scanner.Text())
		event.WriteString("\n")

		if strings.Contains(event.String(), "second=2") {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("the stream never delivered the request:\n%s", event.String())
		}
	}

	streamed := event.String()
	if !strings.Contains(streamed, "event: exchange") {
		t.Errorf("no exchange event in the stream:\n%s", streamed)
	}
	// The row is rendered by the same template the page uses, so it arrives as
	// a table row rather than as JSON the browser would have to assemble.
	if !strings.Contains(streamed, "<tr") {
		t.Errorf("the stream did not send a rendered row:\n%s", streamed)
	}
	// And the exchange that was already there when the stream opened is not
	// sent again.
	if strings.Contains(streamed, "first=1") {
		t.Errorf("the stream replayed an exchange from before it opened:\n%s", streamed)
	}
}

func TestLogStreamNeedsAnAccount(t *testing.T) {
	b := logBrowser(t, newFakeLog(), defaultLogBodyLimit, listEndpoint).noFollow()

	resp, _ := b.get("/projects/checkout/log/stream")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect to the login page", resp.StatusCode)
	}
}

// Paging is by cursor rather than by offset, so a mangled one is refused rather
// than quietly treated as the first page.
func TestLogListRefusesAMangledCursor(t *testing.T) {
	b := logBrowser(t, newFakeLog(), defaultLogBodyLimit, listEndpoint)
	logIn(t, b)

	resp, body := b.get("/projects/checkout/log?before=yesterday")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, "not a page of the log") {
		t.Errorf("unexpected page:\n%s", body)
	}
}

func TestLogListPagesBackwards(t *testing.T) {
	log := newFakeLog()
	b := logBrowser(t, log, defaultLogBodyLimit, listEndpoint)

	// One more than a page, so that the page has an older one to point at.
	for i := range core.DefaultExchangeLimit + 1 {
		log.Record(core.Exchange{
			ProjectID: testProjectID,
			Matched:   true,
			Method:    http.MethodGet,
			Path:      "/orders",
			Query:     "n=" + strconv.Itoa(i),
			CreatedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		})
	}
	logIn(t, b)

	_, body := b.get("/projects/checkout/log")
	if strings.Count(body, `class="log-row`) != core.DefaultExchangeLimit {
		t.Errorf("the first page does not hold exactly %d rows", core.DefaultExchangeLimit)
	}
	// The newest is on the first page and the oldest is not.
	if !strings.Contains(body, "n=50") || strings.Contains(body, "n=0&") {
		t.Errorf("the first page is not the newest entries:\n%s", body)
	}

	older := olderLink(t, body)
	_, page2 := b.get(older)
	if !strings.Contains(page2, "n=0") {
		t.Errorf("the older page does not hold the oldest entry:\n%s", page2)
	}
}

// olderLink pulls the "Older" URL out of a rendered log page.
func olderLink(t *testing.T, body string) string {
	t.Helper()

	const marker = `href="/projects/checkout/log?before=`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no older link on the page:\n%s", body)
	}
	rest := body[i+len(`href="`):]
	return html.UnescapeString(rest[:strings.IndexByte(rest, '"')])
}
