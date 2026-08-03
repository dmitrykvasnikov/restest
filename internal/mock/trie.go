package mock

import (
	"cmp"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// trie is the route table of one project: a radix trie over path segments.
//
// Each node has two kinds of child — the literal segments spelled out in a
// pattern, and the single child that stands for any segment at all. Keeping
// them apart is what gives the precedence rule its teeth: the search descends
// into the literal child first, so /users/me is reached before /users/{id}
// without either of them having to know the other exists.
type trie struct {
	root node
}

type node struct {
	// literal children, keyed by the segment text.
	literal map[string]*node
	// param is the child every segment matches. There is at most one: two
	// patterns that differ only in what they call a parameter describe the same
	// shape, and the shape is what the trie is about.
	param *node
	// routes are the endpoints that end here, keyed by method. A node with no
	// routes is an interior node — a prefix of some pattern, not a pattern.
	routes map[string]*Route
}

// insert adds a route, and reports the route it displaced, if any.
//
// A displacement means two patterns reduce to the same shape and verb —
// /users/{id} and /users/{name} for GET. The database cannot refuse that pair,
// because it compares the pattern text and the text differs. The first one in
// wins, which with the query's `order by path_pattern` makes the choice
// deterministic rather than a coin toss, and the caller reports the other so
// that it is not silently dead.
func (t *trie) insert(r *Route) *Route {
	cur := &t.root

	for _, segment := range core.SplitPath(r.Path) {
		if isParam(segment) {
			if cur.param == nil {
				cur.param = &node{}
			}
			cur = cur.param
			continue
		}
		if cur.literal == nil {
			cur.literal = make(map[string]*node)
		}
		next, ok := cur.literal[segment]
		if !ok {
			next = &node{}
			cur.literal[segment] = next
		}
		cur = next
	}

	if cur.routes == nil {
		cur.routes = make(map[string]*Route)
	}
	if existing, ok := cur.routes[r.Method]; ok {
		return existing
	}
	cur.routes[r.Method] = r
	return nil
}

// isParam reports whether a pattern segment is a named parameter. The pattern
// was validated on the way into the database, so a segment in braces here is a
// parameter and nothing else.
func isParam(segment string) bool {
	return len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}'
}

// lookup finds the route answering method at the given decoded path segments.
//
// It returns the matching route with its parameter values, or, when nothing
// matched, the verbs that this path does answer — which is the difference
// between a 404 and a 405.
func (t *trie) lookup(method string, segments []string) (*Route, map[string]string, []string) {
	s := &search{
		method:   method,
		segments: segments,
		allow:    make(map[string]struct{}),
	}
	t.root.walk(s, 0)

	if s.route == nil {
		return nil, nil, s.allowed()
	}
	return s.route, zip(s.route.Params, s.matched), nil
}

// search is one traversal. It carries the values collected from the parameter
// segments passed on the way down, so that the route finally reached can name
// them.
type search struct {
	method   string
	segments []string

	// values is the running stack, pushed and popped as the walk descends and
	// backtracks. matched is the copy taken at the moment a route was found,
	// kept separate precisely because the walk goes on unwinding — and popping
	// — after that.
	values  []string
	matched []string

	route *Route
	allow map[string]struct{}
}

// walk is depth-first, literal children before the parameter child, stopping at
// the first route that answers.
//
// The backtracking is the point. Preferring the literal child at each step is
// not enough on its own: with /a/b/c and /{x}/b/d defined, a request for
// /a/b/d descends into the literal `a`, runs out of trie at `d`, and has to
// come back up and try the parameter branch. A matcher that committed to the
// literal branch would answer 404 for a route that exists.
func (n *node) walk(s *search, depth int) {
	if s.route != nil {
		return
	}

	if depth == len(s.segments) {
		n.arrive(s)
		return
	}

	segment := s.segments[depth]
	if child, ok := n.literal[segment]; ok {
		child.walk(s, depth+1)
		if s.route != nil {
			return
		}
	}
	if n.param != nil {
		s.values = append(s.values, segment)
		n.param.walk(s, depth+1)
		s.values = s.values[:len(s.values)-1]
	}
}

// arrive handles a node the path reached the end at. It records the route if
// one answers this method, and otherwise notes the verbs that do — the search
// keeps going, so that the Allow header on a 405 covers every pattern the path
// matches rather than the first one found.
func (n *node) arrive(s *search) {
	if len(n.routes) == 0 {
		return
	}

	if r := pick(n.routes, s.method); r != nil {
		s.route = r
		s.matched = slices.Clone(s.values)
		return
	}
	for method := range n.routes {
		s.allow[method] = struct{}{}
	}
}

// pick chooses which of a node's routes answers method.
//
// An exact verb beats the wildcard, so defining GET alongside * narrows the
// wildcard rather than being shadowed by it. HEAD falls back to GET last of
// all: a client asking for the headers of a resource it can GET should get
// them, and net/http discards the body of a HEAD response for us.
func pick(routes map[string]*Route, method string) *Route {
	if r, ok := routes[method]; ok {
		return r
	}
	if r, ok := routes[core.MethodAny]; ok {
		return r
	}
	if method == http.MethodHead {
		if r, ok := routes[http.MethodGet]; ok {
			return r
		}
	}
	return nil
}

// allowed renders the verbs collected during a failed search, in the order
// Allow conventionally lists them.
//
// HEAD is added wherever GET is, because pick answers HEAD from a GET route and
// the header has to describe what the server will actually do. The wildcard
// cannot appear: a node carrying * would have matched.
func (s *search) allowed() []string {
	if len(s.allow) == 0 {
		return nil
	}
	if _, ok := s.allow[http.MethodGet]; ok {
		s.allow[http.MethodHead] = struct{}{}
	}

	methods := slices.Collect(maps.Keys(s.allow))
	slices.SortFunc(methods, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(methodOrder(a), methodOrder(b)),
			cmp.Compare(a, b),
		)
	})
	return methods
}

// allowOrder is the order an Allow header conventionally lists verbs in —
// safe first, then the ones that change something. It is not core.Methods,
// which is the order the form offers them in and puts HEAD near the end.
var allowOrder = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// methodOrder places a verb in that order, so a header reads GET, HEAD, POST
// rather than in whatever order a map yielded.
func methodOrder(method string) int {
	if i := slices.Index(allowOrder, method); i >= 0 {
		return i
	}
	return len(allowOrder)
}

// zip pairs parameter names with the values the search collected. The two are
// the same length by construction — a route's names come from the same segments
// the walk descended through — but a mismatch would be a bug worth not
// panicking over, so the shorter one wins.
func zip(names, values []string) map[string]string {
	if len(names) == 0 {
		return nil
	}

	params := make(map[string]string, len(names))
	for i, name := range names {
		if i >= len(values) {
			break
		}
		params[name] = values[i]
	}
	return params
}

// splitRequestPath breaks the *escaped* path of a request into decoded
// segments.
//
// It must be the escaped path. r.URL.Path has already had %2F turned into a
// slash, so matching on it would split /users/a%2Fb into three segments and
// hand the endpoint a parameter it never received. Splitting first and decoding
// each segment afterwards keeps an encoded slash inside the segment it was
// written in.
func splitRequestPath(escaped string) []string {
	segments := core.SplitPath(escaped)
	for i, segment := range segments {
		if !strings.ContainsRune(segment, '%') {
			continue
		}
		if decoded, err := url.PathUnescape(segment); err == nil {
			segments[i] = decoded
		}
		// A segment that will not decode is left as it stands. net/url has
		// already parsed the request line, so this is close to unreachable, and
		// the raw text is a better thing to try to match than an error page.
	}
	return segments
}
