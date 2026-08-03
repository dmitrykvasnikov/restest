package core

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestParseListQueryDefaults(t *testing.T) {
	q, err := ParseListQuery(url.Values{})
	if err != nil {
		t.Fatalf("ParseListQuery: %v", err)
	}

	if q.Page != 1 {
		t.Errorf("page = %d, want 1", q.Page)
	}
	if q.Limit != DefaultListLimit {
		t.Errorf("limit = %d, want %d", q.Limit, DefaultListLimit)
	}
	if q.Sort != "" || q.Desc {
		t.Errorf("sort = %q desc = %v, want insertion order ascending", q.Sort, q.Desc)
	}
	if len(q.Filters) != 0 {
		t.Errorf("filters = %v, want none", q.Filters)
	}
}

func TestParseListQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  ListQuery
	}{
		{
			name:  "the listing parameters",
			query: "_page=3&_limit=10&_sort=name&_order=desc",
			want:  ListQuery{Page: 3, Limit: 10, Sort: "name", Desc: true},
		},
		{
			name:  "asc is the default and may be said out loud",
			query: "_order=ASC",
			want:  ListQuery{Page: 1, Limit: DefaultListLimit},
		},
		{
			name:  "anything without an underscore is a filter",
			query: "status=active",
			want: ListQuery{Page: 1, Limit: DefaultListLimit,
				Filters: []Filter{{Field: "status", Values: []string{"active"}}}},
		},
		{
			name:  "repeating a field asks for either value",
			query: "status=active&status=trial",
			want: ListQuery{Page: 1, Limit: DefaultListLimit,
				Filters: []Filter{{Field: "status", Values: []string{"active", "trial"}}}},
		},
		{
			// Filters come back sorted so that one query string always builds the
			// same statement, whatever order the map yielded.
			name:  "several fields are ordered by name",
			query: "role=admin&city=Kyiv",
			want: ListQuery{Page: 1, Limit: DefaultListLimit, Filters: []Filter{
				{Field: "city", Values: []string{"Kyiv"}},
				{Field: "role", Values: []string{"admin"}},
			}},
		},
		{
			// A field called `page` is a field, not a listing parameter. That is
			// the whole reason the reserved names carry an underscore.
			name:  "a field may be called page",
			query: "page=2",
			want: ListQuery{Page: 1, Limit: DefaultListLimit,
				Filters: []Filter{{Field: "page", Values: []string{"2"}}}},
		},
		{
			name:  "an empty value is still a filter",
			query: "note=",
			want: ListQuery{Page: 1, Limit: DefaultListLimit,
				Filters: []Filter{{Field: "note", Values: []string{""}}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("parse the test's own query: %v", err)
			}

			got, err := ParseListQuery(values)
			if err != nil {
				t.Fatalf("ParseListQuery(%q): %v", tt.query, err)
			}
			if got.Page != tt.want.Page || got.Limit != tt.want.Limit ||
				got.Sort != tt.want.Sort || got.Desc != tt.want.Desc {
				t.Errorf("ParseListQuery(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
			if len(got.Filters) != len(tt.want.Filters) {
				t.Fatalf("filters = %v, want %v", got.Filters, tt.want.Filters)
			}
			for i, filter := range got.Filters {
				if filter.Field != tt.want.Filters[i].Field ||
					!slices.Equal(filter.Values, tt.want.Filters[i].Values) {
					t.Errorf("filter %d = %+v, want %+v", i, filter, tt.want.Filters[i])
				}
			}
		})
	}
}

// A query the server will not guess at is refused with a message, not answered
// with something plausible. Returning the first hundred documents to a client
// that asked for `_limits=5` would look as though the parameter had worked.
func TestParseListQueryRefusals(t *testing.T) {
	tests := []struct {
		name  string
		query string
		says  string
	}{
		{name: "page is not a number", query: "_page=first", says: "_page"},
		{name: "page counts from one", query: "_page=0", says: "_page"},
		{name: "page is not negative", query: "_page=-1", says: "_page"},
		{name: "limit is not a number", query: "_limit=lots", says: "_limit"},
		{name: "limit has a ceiling", query: "_limit=10001", says: "_limit"},
		{name: "limit is not zero", query: "_limit=0", says: "_limit"},
		{name: "order is one of two words", query: "_order=sideways", says: "_order"},
		{name: "an unknown listing parameter is a typo", query: "_limits=5", says: "_limits"},
		{name: "even a plausible one", query: "_embed=posts", says: "_embed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("parse the test's own query: %v", err)
			}

			_, err = ParseListQuery(values)
			if err == nil {
				t.Fatalf("ParseListQuery(%q) was accepted", tt.query)
			}

			var qe QueryError
			if !asQueryError(err, &qe) {
				t.Fatalf("error is %T, want QueryError so the handler can answer 400", err)
			}
			if !strings.Contains(qe.Message, tt.says) {
				t.Errorf("message %q does not name %q", qe.Message, tt.says)
			}
		})
	}
}

func asQueryError(err error, target *QueryError) bool {
	qe, ok := err.(QueryError) //nolint:errorlint // the type is returned directly
	if ok {
		*target = qe
	}
	return ok
}

// A query string has no types. A value that also reads as a number is matched
// both ways, so ?id=1 finds {"id":1} and {"id":"1"} alike.
func TestContainmentDocs(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		want  []string
	}{
		{
			name:  "a word is only a string",
			field: "status", value: "active",
			want: []string{`{"status":"active"}`},
		},
		{
			name: "a number is both", field: "id", value: "7",
			want: []string{`{"id":"7"}`, `{"id":7}`},
		},
		{
			name: "so is a negative one", field: "balance", value: "-3",
			want: []string{`{"balance":"-3"}`, `{"balance":-3}`},
		},
		{
			name: "and a decimal", field: "score", value: "1.5",
			want: []string{`{"score":"1.5"}`, `{"score":1.5}`},
		},
		{
			name: "true is both", field: "active", value: "true",
			want: []string{`{"active":"true"}`, `{"active":true}`},
		},
		{
			name: "null is both", field: "deleted_at", value: "null",
			want: []string{`{"deleted_at":"null"}`, `{"deleted_at":null}`},
		},
		{
			// Not a JSON scalar, so only the string reading. A leading zero is
			// how a product code is written, and it is not the number 7.
			name: "a padded number stays text", field: "code", value: "007",
			want: []string{`{"code":"007"}`},
		},
		{
			name: "a field name needing escaping survives", field: `a"b`, value: "x",
			want: []string{`{"a\"b":"x"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containmentDocs(tt.field, tt.value)
			if !slices.Equal(got, tt.want) {
				t.Errorf("containmentDocs(%q, %q) = %q, want %q", tt.field, tt.value, got, tt.want)
			}
		})
	}
}

// The identifier written into a document keeps the JSON type a client would
// expect: /users/1 carries {"id": 1}, and a uuid carries a string.
func TestIDJSON(t *testing.T) {
	tests := []struct{ in, want string }{
		{"1", "1"},
		{"42", "42"},
		{"-1", "-1"},
		{"0", "0"},
		{"007", `"007"`},
		{"7a", `"7a"`},
		{"", `""`},
		{"6b7e2c4e-0000-4000-8000-000000000000", `"6b7e2c4e-0000-4000-8000-000000000000"`},
		// Beyond int64, so it is not a number this server will claim to hold.
		{"99999999999999999999", `"99999999999999999999"`},
	}

	for _, tt := range tests {
		if got := idJSON(tt.in); got != tt.want {
			t.Errorf("idJSON(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestCheckObject(t *testing.T) {
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{name: "an object", body: `{"a":1}`, ok: true},
		{name: "an empty object", body: `{}`, ok: true},
		{name: "leading space", body: "  \n{\"a\":1}", ok: true},
		{name: "an array is not a document", body: `[{"a":1}]`},
		{name: "nor is a number", body: `7`},
		{name: "nor a string", body: `"hello"`},
		{name: "nor nothing at all", body: ``},
		{name: "nor half an object", body: `{"a":`},
		{name: "nor two of them", body: `{"a":1}{"b":2}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkObject([]byte(tt.body))
			if tt.ok && err != nil {
				t.Errorf("checkObject(%q) = %v, want accepted", tt.body, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("checkObject(%q) was accepted", tt.body)
			}
		})
	}
}
