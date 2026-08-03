package mock

import (
	"cmp"
	"slices"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// nearestLimit is how many suggestions a 404 body carries. Enough to cover a
// typo and a wrong verb; few enough that the answer is still readable.
const nearestLimit = 5

// nearest ranks a project's routes by how close they look to what was asked
// for.
//
// The ranking is deliberately crude, because it is a hint and not a search. Two
// things decide it: how far along the path the request and the pattern still
// agree, and how many single-character edits separate the two paths. The first
// dominates — someone who asked for /users/1/postz has a prefix in common with
// /users/{id}/posts and none at all with /orders, and the prefix is the more
// useful signal. Edit distance settles the rest.
func nearest(routes []Ref, method, path string, limit int) []Ref {
	if len(routes) == 0 {
		return nil
	}

	wanted := core.SplitPath(path)
	scored := make([]scoredRef, len(routes))
	for i, ref := range routes {
		scored[i] = scoredRef{
			Ref:    ref,
			prefix: commonPrefix(wanted, core.SplitPath(ref.Path)),
			edits:  editDistance(path, ref.Path),
			// A route answering the verb that was asked for is more likely to
			// be the one meant, but only as a tie-break: the path is what was
			// probably mistyped.
			method: boolOrder(ref.Method != method && ref.Method != core.MethodAny),
		}
	}

	slices.SortFunc(scored, func(a, b scoredRef) int {
		return cmp.Or(
			cmp.Compare(b.prefix, a.prefix),
			cmp.Compare(a.edits, b.edits),
			cmp.Compare(a.method, b.method),
			cmp.Compare(a.Path, b.Path),
			cmp.Compare(a.Method, b.Method),
		)
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}
	refs := make([]Ref, len(scored))
	for i, s := range scored {
		refs[i] = s.Ref
	}
	return refs
}

type scoredRef struct {
	Ref
	prefix int
	edits  int
	method int
}

func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

// commonPrefix counts the leading segments that agree, where a parameter
// segment agrees with anything — which is what it would have done had the rest
// of the pattern matched.
func commonPrefix(wanted, pattern []string) int {
	n := min(len(wanted), len(pattern))
	for i := range n {
		if pattern[i] != wanted[i] && !isParam(pattern[i]) {
			return i
		}
	}
	return n
}

// editDistance is Levenshtein distance, two rows at a time.
//
// Paths are short and a project's route list is small, so the O(n·m) of the
// straightforward version is not worth avoiding; keeping only two rows is
// enough not to allocate a matrix per candidate.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) < len(br) {
		ar, br = br, ar
	}
	if len(br) == 0 {
		return len(ar)
	}

	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}
