//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// anExchange is a complete record, so that every column — including the ones
// most exchanges leave empty — is written and read back at least once.
func anExchange(projectID uuid.UUID) core.Exchange {
	return core.Exchange{
		ID:         uuid.New(),
		ProjectID:  projectID,
		EndpointID: uuid.New(),
		Direction:  core.DirectionInbound,
		Matched:    true,
		Method:     http.MethodPost,
		Path:       "/orders",
		Query:      "trace=7",
		RequestHeaders: core.HeaderSet{
			"Content-Type":  {"application/json"},
			"Accept":        {"application/json", "text/plain"},
			"Authorization": {"Bearer sekrit"},
		},
		RequestBody:           []byte(`{"sku":"abc"}`),
		StatusCode:            201,
		ResponseHeaders:       core.HeaderSet{"X-Mock": {"yes"}},
		ResponseBody:          []byte(`{"ok":true}`),
		ResponseBodyTruncated: true,
		Duration:              37 * time.Millisecond,
		RemoteAddr:            "192.0.2.10",
		CreatedAt:             time.Now(),
	}
}

// The whole round trip through the real column types: jsonb headers, bytea
// bodies, an inet address, a partitioned table and a COPY.
func TestExchangeRoundTrip(t *testing.T) {
	store, _, project := newStoreWithProject(t)
	want := anExchange(project.ID)

	if n, err := store.InsertExchanges(t.Context(), []core.Exchange{want}); err != nil || n != 1 {
		t.Fatalf("InsertExchanges = %d, %v", n, err)
	}

	got, err := store.ExchangeByID(t.Context(), project.ID, want.ID)
	if err != nil {
		t.Fatalf("ExchangeByID: %v", err)
	}

	if got.Method != want.Method || got.Path != want.Path || got.Query != want.Query {
		t.Errorf("request line = %s %s?%s, want %s %s?%s",
			got.Method, got.Path, got.Query, want.Method, want.Path, want.Query)
	}
	if got.EndpointID != want.EndpointID || !got.Matched {
		t.Errorf("endpoint = %s, matched = %v", got.EndpointID, got.Matched)
	}
	if got.StatusCode != 201 {
		t.Errorf("StatusCode = %d, want 201", got.StatusCode)
	}
	if string(got.RequestBody) != string(want.RequestBody) {
		t.Errorf("RequestBody = %q, want %q", got.RequestBody, want.RequestBody)
	}
	if string(got.ResponseBody) != string(want.ResponseBody) {
		t.Errorf("ResponseBody = %q, want %q", got.ResponseBody, want.ResponseBody)
	}
	if !got.ResponseBodyTruncated {
		t.Error("ResponseBodyTruncated was not stored")
	}
	if got.Duration != 37*time.Millisecond {
		t.Errorf("Duration = %v, want 37ms", got.Duration)
	}
	if got.RemoteAddr != "192.0.2.10" {
		t.Errorf("RemoteAddr = %q, want the address it was given", got.RemoteAddr)
	}
	// Repeated headers survive as repeats: the log is a record of what was
	// sent, and a client that sent two Accepts sent two.
	if v := got.RequestHeaders.Values("Accept"); v != "application/json, text/plain" {
		t.Errorf("Accept = %q, want both values", v)
	}
	if v := got.ResponseHeaders.Values("X-Mock"); v != "yes" {
		t.Errorf("X-Mock = %q, want yes", v)
	}
	// And the credential does not: redaction happens at the storage boundary,
	// so what is in the table is already redacted.
	if v := got.RequestHeaders.Values("Authorization"); v != "Bearer [redacted]" {
		t.Errorf("stored Authorization = %q, want the credential replaced", v)
	}
}

// An exchange that matched nothing has no endpoint, no query, no status and no
// address to record. Every one of those columns is nullable, and this is what
// proves the Go side agrees with the schema about it.
func TestExchangeWithNothingOptional(t *testing.T) {
	store, _, project := newStoreWithProject(t)

	bare := core.Exchange{
		ID:        uuid.New(),
		ProjectID: project.ID,
		Method:    http.MethodGet,
		Path:      "/nope",
		CreatedAt: time.Now(),
	}
	if _, err := store.InsertExchanges(t.Context(), []core.Exchange{bare}); err != nil {
		t.Fatalf("InsertExchanges: %v", err)
	}

	got, err := store.ExchangeByID(t.Context(), project.ID, bare.ID)
	if err != nil {
		t.Fatalf("ExchangeByID: %v", err)
	}
	if got.EndpointID != uuid.Nil || got.Query != "" || got.StatusCode != 0 || got.RemoteAddr != "" {
		t.Errorf("empty columns came back as %+v", got)
	}
	if got.Direction != core.DirectionInbound {
		t.Errorf("Direction = %q, want inbound by default", got.Direction)
	}
	if got.RequestHeaders == nil || got.ResponseHeaders == nil {
		t.Error("headers came back nil rather than empty")
	}
}

// One project's log is not another's, and an id from somebody else's project is
// the same answer as one that never existed.
func TestExchangesAreScopedToTheirProject(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	other, err := store.CreateProject(t.Context(), user.ID, "billing", "Billing API", nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	mine := anExchange(project.ID)
	theirs := anExchange(other.ID)
	if _, err := store.InsertExchanges(t.Context(), []core.Exchange{mine, theirs}); err != nil {
		t.Fatalf("InsertExchanges: %v", err)
	}

	page, err := store.ExchangesByProject(t.Context(), project.ID, core.ExchangeCursor{}, 10)
	if err != nil {
		t.Fatalf("ExchangesByProject: %v", err)
	}
	if len(page) != 1 || page[0].ID != mine.ID {
		t.Fatalf("listed %d exchanges, want only this project's", len(page))
	}

	if _, err := store.ExchangeByID(t.Context(), project.ID, theirs.ID); err != core.ErrNotFound {
		t.Errorf("ExchangeByID for another project's exchange = %v, want ErrNotFound", err)
	}
}

// Paging is by cursor, and the cursor is a pair, so exchanges sharing a
// timestamp to the microsecond still come back exactly once each.
func TestExchangePagingReturnsEachExactlyOnce(t *testing.T) {
	store, _, project := newStoreWithProject(t)

	// Every one of them at the same instant — which is not contrived: a batch
	// of them arrives from one COPY, and a busy mock server produces them
	// faster than the clock ticks.
	at := time.Now().Truncate(time.Millisecond)
	batch := make([]core.Exchange, 25)
	for i := range batch {
		batch[i] = core.Exchange{
			ID:        uuid.New(),
			ProjectID: project.ID,
			Method:    http.MethodGet,
			Path:      fmt.Sprintf("/orders/%d", i),
			CreatedAt: at,
		}
	}
	if _, err := store.InsertExchanges(t.Context(), batch); err != nil {
		t.Fatalf("InsertExchanges: %v", err)
	}

	seen := make(map[uuid.UUID]int)
	var cursor core.ExchangeCursor
	for range 10 {
		page, err := store.ExchangesByProject(t.Context(), project.ID, cursor, 7)
		if err != nil {
			t.Fatalf("ExchangesByProject: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ex := range page {
			seen[ex.ID]++
		}
		cursor = page[len(page)-1].Cursor()
	}

	if len(seen) != len(batch) {
		t.Errorf("paged through %d exchanges, want all %d", len(seen), len(batch))
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("exchange %s came back %d times", id, times)
		}
	}
}

// The tail asks for everything after a cursor, and must not include the row the
// cursor names — which is the same off-by-one paging has, from the other side.
func TestExchangesSinceExcludesTheCursorItself(t *testing.T) {
	store, _, project := newStoreWithProject(t)

	at := time.Now()
	first := core.Exchange{ID: uuid.New(), ProjectID: project.ID, Method: "GET", Path: "/first", CreatedAt: at}
	second := core.Exchange{ID: uuid.New(), ProjectID: project.ID, Method: "GET", Path: "/second", CreatedAt: at.Add(time.Millisecond)}

	if _, err := store.InsertExchanges(t.Context(), []core.Exchange{first, second}); err != nil {
		t.Fatalf("InsertExchanges: %v", err)
	}

	cursor, err := store.LatestExchangeCursor(t.Context(), project.ID)
	if err != nil {
		t.Fatalf("LatestExchangeCursor: %v", err)
	}
	if cursor.ID != second.ID {
		t.Fatalf("the newest exchange is %s, want the second one", cursor.ID)
	}

	// From the newest, nothing is newer.
	tail, err := store.ExchangesSince(t.Context(), project.ID, cursor, 10)
	if err != nil {
		t.Fatalf("ExchangesSince: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("the tail returned %d exchanges from the newest cursor, want none", len(tail))
	}

	// From the first, only the second.
	tail, err = store.ExchangesSince(t.Context(), project.ID, first.Cursor(), 10)
	if err != nil {
		t.Fatalf("ExchangesSince: %v", err)
	}
	if len(tail) != 1 || tail[0].ID != second.ID {
		t.Fatalf("the tail returned %d exchanges, want only the second", len(tail))
	}
}

// A project with nothing recorded has no cursor, and that is not an error: it
// is a log that has not started.
func TestLatestCursorOfAQuietProject(t *testing.T) {
	store, _, project := newStoreWithProject(t)

	cursor, err := store.LatestExchangeCursor(t.Context(), project.ID)
	if err != nil {
		t.Fatalf("LatestExchangeCursor: %v", err)
	}
	if !cursor.IsZero() {
		t.Errorf("cursor = %v, want the zero cursor", cursor)
	}
}

// Deleting a project does not cascade into its log: exchanges deliberately
// carry no foreign keys, because rows leave that table by dropping a partition.
// The rows are unreachable through the UI, and this is what proves the schema
// means it.
func TestDeletingAProjectLeavesItsExchanges(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	if _, err := store.InsertExchanges(t.Context(), []core.Exchange{anExchange(project.ID)}); err != nil {
		t.Fatalf("InsertExchanges: %v", err)
	}
	if err := store.DeleteProject(t.Context(), user.ID, project.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	page, err := store.ExchangesByProject(t.Context(), project.ID, core.ExchangeCursor{}, 10)
	if err != nil {
		t.Fatalf("ExchangesByProject: %v", err)
	}
	if len(page) != 1 {
		t.Errorf("the deleted project's exchanges are gone; the log has no foreign keys and should not cascade")
	}
}

// --- partitions ---------------------------------------------------------

func TestPartitionsAreCreatedAhead(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	if err := store.CreateExchangePartitions(t.Context(), now, 2); err != nil {
		t.Fatalf("CreateExchangePartitions: %v", err)
	}

	for _, want := range []string{"exchanges_2026_08", "exchanges_2026_09", "exchanges_2026_10"} {
		if !hasPartition(t, store, want) {
			t.Errorf("partition %s was not created", want)
		}
	}
	if hasPartition(t, store, "exchanges_2026_11") {
		t.Error("a partition beyond the requested months was created")
	}

	// Running it twice is what a restart does, and it has to be a no-op rather
	// than a failure.
	if err := store.CreateExchangePartitions(t.Context(), now, 2); err != nil {
		t.Fatalf("CreateExchangePartitions a second time: %v", err)
	}
}

// A row lands in the partition its timestamp belongs to, which is what makes
// dropping a month a way of expiring exactly that month.
func TestRowsLandInTheirMonthsPartition(t *testing.T) {
	store, _, project := newStoreWithProject(t)
	now := time.Now().UTC()

	if err := store.CreateExchangePartitions(t.Context(), now, 1); err != nil {
		t.Fatalf("CreateExchangePartitions: %v", err)
	}

	ex := anExchange(project.ID)
	ex.CreatedAt = now
	if _, err := store.InsertExchanges(t.Context(), []core.Exchange{ex}); err != nil {
		t.Fatalf("InsertExchanges: %v", err)
	}

	partition := fmt.Sprintf("exchanges_%04d_%02d", now.Year(), int(now.Month()))
	var n int
	if err := store.Pool().QueryRow(t.Context(),
		"select count(*) from "+partition+" where id = $1", ex.ID).Scan(&n); err != nil {
		t.Fatalf("count rows in %s: %v", partition, err)
	}
	if n != 1 {
		t.Errorf("the row is not in %s", partition)
	}
	// And it is still visible through the parent, which is the point of a
	// partitioned table.
	if _, err := store.ExchangeByID(t.Context(), project.ID, ex.ID); err != nil {
		t.Errorf("the row is not readable through the parent table: %v", err)
	}
}

// A month with no partition of its own still accepts writes: the default
// partition is what stops logging from ever being the reason a mock request
// fails.
func TestTheDefaultPartitionCatchesWhatIsUncovered(t *testing.T) {
	store, _, project := newStoreWithProject(t)

	// Far enough ahead that no maintenance run has covered it.
	ex := anExchange(project.ID)
	ex.CreatedAt = time.Now().AddDate(5, 0, 0)

	if _, err := store.InsertExchanges(t.Context(), []core.Exchange{ex}); err != nil {
		t.Fatalf("InsertExchanges into an uncovered month: %v", err)
	}

	var n int
	if err := store.Pool().QueryRow(t.Context(),
		"select count(*) from exchanges_default where id = $1", ex.ID).Scan(&n); err != nil {
		t.Fatalf("count rows in the default partition: %v", err)
	}
	if n != 1 {
		t.Error("the row did not land in the default partition")
	}
}

// Retention: the months outside the window are detached and dropped, the ones
// inside it are left alone, and the default partition is never touched.
func TestRetentionDropsExpiredPartitions(t *testing.T) {
	store, _, project := newStoreWithProject(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	// Five months, one exchange in each.
	for i := range 5 {
		month := now.AddDate(0, -i, 0)
		if err := store.CreateExchangePartitions(t.Context(), month, 0); err != nil {
			t.Fatalf("CreateExchangePartitions: %v", err)
		}

		ex := anExchange(project.ID)
		ex.CreatedAt = month
		if _, err := store.InsertExchanges(t.Context(), []core.Exchange{ex}); err != nil {
			t.Fatalf("InsertExchanges: %v", err)
		}
	}

	dropped, err := store.DropExpiredExchangePartitions(t.Context(), now, 3)
	if err != nil {
		t.Fatalf("DropExpiredExchangePartitions: %v", err)
	}

	if len(dropped) != 2 {
		t.Fatalf("dropped %v, want the two months outside a three-month window", dropped)
	}
	for _, gone := range []string{"exchanges_2026_04", "exchanges_2026_05"} {
		if !contains(dropped, gone) {
			t.Errorf("%s was not dropped", gone)
		}
		if hasPartition(t, store, gone) {
			t.Errorf("%s is still attached", gone)
		}
		if hasTable(t, store, gone) {
			t.Errorf("%s was detached but not dropped, leaving an orphan table", gone)
		}
	}
	for _, kept := range []string{"exchanges_2026_06", "exchanges_2026_07", "exchanges_2026_08"} {
		if !hasPartition(t, store, kept) {
			t.Errorf("%s is inside the window and was dropped anyway", kept)
		}
	}
	// The safety net is never swept away with the rest.
	if !hasPartition(t, store, "exchanges_default") {
		t.Error("the default partition was dropped")
	}

	// Three months of exchanges remain, and the expired ones are gone with
	// their partitions.
	var n int
	if err := store.Pool().QueryRow(t.Context(), "select count(*) from exchanges").Scan(&n); err != nil {
		t.Fatalf("count exchanges: %v", err)
	}
	if n != 3 {
		t.Errorf("%d exchanges remain, want the 3 inside the window", n)
	}
}

// However short a window is asked for, the month being written into stays:
// detaching it would send live traffic to the default partition.
func TestRetentionNeverDropsTheCurrentMonth(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()

	if err := store.CreateExchangePartitions(t.Context(), now, 0); err != nil {
		t.Fatalf("CreateExchangePartitions: %v", err)
	}

	if _, err := store.DropExpiredExchangePartitions(t.Context(), now, 0); err != nil {
		t.Fatalf("DropExpiredExchangePartitions: %v", err)
	}

	current := fmt.Sprintf("exchanges_%04d_%02d", now.Year(), int(now.Month()))
	if !hasPartition(t, store, current) {
		t.Errorf("%s was dropped; the month being written into must survive any window", current)
	}
}

// MaintainExchangeLog is the two halves together, and it is what runs at
// startup — so it has to be safe on a database where it has never run and on
// one where it has.
func TestMaintenanceIsIdempotent(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()

	for range 2 {
		if err := store.MaintainExchangeLog(t.Context(), now, core.DefaultRetentionMonths); err != nil {
			t.Fatalf("MaintainExchangeLog: %v", err)
		}
	}

	month := now
	for range core.PartitionsAhead + 1 {
		want := fmt.Sprintf("exchanges_%04d_%02d", month.Year(), int(month.Month()))
		if !hasPartition(t, store, want) {
			t.Errorf("partition %s was not created", want)
		}
		month = month.AddDate(0, 1, 0)
	}
}

// --- the milestone ------------------------------------------------------

// TestTheM4Milestone is M4's "done when": requests appear in the UI as they
// arrive, without a refresh. Everything it touches is real — Postgres, the
// partitioned table, the recorder's queue and batching writer, the route table,
// the templates, the session and the SSE stream.
func TestTheM4Milestone(t *testing.T) {
	s := newSite(t)
	registerAndCreateProject(t, s)

	// --- define an endpoint through the form -----------------------------
	if resp, body := s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints",
		formValues(map[string]string{
			"method": "POST", "path": "/orders", "status_code": "201",
			"delay_ms": "0", "is_enabled": "1", "headers": "X-Mock: yes",
			"body": `{"ok":true}`,
		})); resp.StatusCode != http.StatusOK {
		t.Fatalf("create endpoint: status = %d\n%s", resp.StatusCode, body)
	}

	// --- send it something, as a client under test would ------------------
	if resp, body := s.send(http.MethodPost, "/m/checkout/orders?trace=7", `{"sku":"abc"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST to the mock: status = %d\n%s", resp.StatusCode, body)
	}

	// --- it is written down, off the request path ------------------------
	waitForExchanges(t, s, 1)

	// --- and it is on the page -------------------------------------------
	resp, page := s.get("/projects/checkout/log")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("log page: status = %d\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{"/orders?trace=7", "POST", "201"} {
		if !strings.Contains(page, want) {
			t.Errorf("the log page does not show %q:\n%s", want, page)
		}
	}

	// --- the detail view has the bodies and the headers -------------------
	id := s.value(`select id::text from exchanges order by created_at desc limit 1`)
	resp, detail := s.get("/projects/checkout/log/" + id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange page: status = %d\n%s", resp.StatusCode, detail)
	}
	if !strings.Contains(detail, "sku") || !strings.Contains(detail, "X-Mock") {
		t.Errorf("the exchange page is missing the body or the headers:\n%s", detail)
	}

	// --- and the next request arrives without a refresh -------------------
	//
	// The stream never ends on its own, so the read is bounded here rather than
	// left to the test binary's own timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	stream, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.url+"/projects/checkout/log/stream", nil)
	if err != nil {
		t.Fatalf("build the stream request: %v", err)
	}

	resp, err = s.client.Do(stream)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test client response

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q, want text/event-stream", got)
	}

	// Sent after the stream is open, which is the sequence a user sees.
	if resp, body := s.send(http.MethodPost, "/m/checkout/orders?trace=8", `{"sku":"def"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second POST: status = %d\n%s", resp.StatusCode, body)
	}

	streamed := readUntil(t, resp.Body, "trace=8")
	if !strings.Contains(streamed, "event: exchange") {
		t.Errorf("the stream did not deliver the request:\n%s", streamed)
	}
	if strings.Contains(streamed, "trace=7") {
		t.Errorf("the stream replayed what was already on the page:\n%s", streamed)
	}
}

// readUntil reads a stream up to the line containing marker, returning
// everything it read. A stream that ends without it is a failure — that is the
// live tail not delivering.
func readUntil(t *testing.T, body io.Reader, marker string) string {
	t.Helper()

	var read strings.Builder
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		read.WriteString(scanner.Text())
		read.WriteString("\n")

		if strings.Contains(scanner.Text(), marker) {
			return read.String()
		}
	}
	t.Fatalf("the stream ended without %q:\n%s", marker, read.String())
	return ""
}

// waitForExchanges waits for the recorder to have written n rows. The write is
// off the request path by design, so a test that looked immediately would be
// testing the timing rather than the behaviour.
func waitForExchanges(t *testing.T, s *site, n int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.count(`select count(*) from exchanges`) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d exchanges to be written", n)
}

func hasPartition(t *testing.T, store *core.Store, name string) bool {
	t.Helper()

	var n int
	err := store.Pool().QueryRow(t.Context(), `
		select count(*)
		from pg_inherits
		join pg_class parent on parent.oid = pg_inherits.inhparent
		join pg_class child  on child.oid  = pg_inherits.inhrelid
		where parent.relname = 'exchanges' and child.relname = $1`, name).Scan(&n)
	if err != nil {
		t.Fatalf("look for partition %s: %v", name, err)
	}
	return n > 0
}

func hasTable(t *testing.T, store *core.Store, name string) bool {
	t.Helper()

	var n int
	err := store.Pool().QueryRow(t.Context(),
		`select count(*) from pg_class where relname = $1 and relkind = 'r'`, name).Scan(&n)
	if err != nil {
		t.Fatalf("look for table %s: %v", name, err)
	}
	return n > 0
}
