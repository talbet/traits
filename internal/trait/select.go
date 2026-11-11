package trait

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"sync"
)

// maxAlternatives caps how many patterns one glob's {a,b} groups may expand to.
const maxAlternatives = 256

// Matches reports whether the trait applies to the slash-separated,
// repo-relative path. A trait without appliesTo applies everywhere.
//
// Globs support "*", "?", "[...]" (as in path.Match), "{a,b}" alternation, and
// "**" as a whole segment matching zero or more segments. A glob without "/"
// matches either the basename or the full path, so "*.go" matches
// "cmd/main.go".
func (t Trait) Matches(name string) bool {
	if len(t.AppliesTo) == 0 {
		return true
	}
	name = strings.TrimPrefix(name, "./")
	for _, g := range t.AppliesTo {
		if matchGlob(g, name) {
			return true
		}
	}
	return false
}

// Gate reports whether any of the trait's when regexes matches any of the
// changed lines. A trait without when always passes the gate. Regexes that do
// not compile (possible only for traits not built by Parse) never match.
func (t Trait) Gate(changed []string) bool {
	if len(t.When) == 0 {
		return true
	}
	for _, w := range t.When {
		re, err := compileWhen(w)
		if err != nil {
			continue
		}
		for _, line := range changed {
			if re.MatchString(line) {
				return true
			}
		}
	}
	return false
}

// whenCache holds compiled when regexes keyed by source, so each pattern is
// compiled once per process however many times a trait is copied or gated.
var whenCache sync.Map // string -> *regexp.Regexp

func compileWhen(expr string) (*regexp.Regexp, error) {
	if re, ok := whenCache.Load(expr); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	whenCache.Store(expr, re)
	return re, nil
}

// matchGlob reports whether glob matches name. Invalid globs never match.
func matchGlob(glob, name string) bool {
	alts, err := expandBraces(glob)
	if err != nil {
		return false
	}
	segs := strings.Split(name, "/")
	for _, alt := range alts {
		alt = strings.TrimPrefix(alt, "/")
		pat := strings.Split(alt, "/")
		if matchSegments(pat, segs) {
			return true
		}
		if !strings.Contains(alt, "/") && matchSegments(pat, segs[len(segs)-1:]) {
			return true
		}
	}
	return false
}

// validGlob reports whether glob is a well-formed pattern.
func validGlob(glob string) error {
	if glob == "" {
		return errors.New("empty pattern")
	}
	alts, err := expandBraces(glob)
	if err != nil {
		return err
	}
	for _, alt := range alts {
		for _, seg := range strings.Split(alt, "/") {
			if seg == "**" {
				continue
			}
			if _, err := path.Match(seg, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// matchSegments matches path segments against pattern segments, where a "**"
// pattern segment matches zero or more path segments. It is the classic
// wildcard algorithm with one backtrack point, so it runs in O(len(pat)*len(segs)).
func matchSegments(pat, segs []string) bool {
	p, s := 0, 0
	starP, starS := -1, 0
	for s < len(segs) {
		switch {
		case p < len(pat) && pat[p] == "**":
			starP, starS = p, s
			p++
		case p < len(pat) && matchSegment(pat[p], segs[s]):
			p++
			s++
		case starP >= 0:
			starS++
			p, s = starP+1, starS
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == "**" {
		p++
	}
	return p == len(pat)
}

func matchSegment(pat, seg string) bool {
	ok, err := path.Match(pat, seg)
	return err == nil && ok
}

// expandBraces expands "{a,b}" groups (nested allowed) into the patterns they
// stand for. Braces inside "[...]" or escaped with "\" are literal.
func expandBraces(glob string) ([]string, error) {
	open, close, commas, err := findBraces(glob)
	if err != nil {
		return nil, err
	}
	if open < 0 {
		return []string{glob}, nil
	}
	prefix, suffix := glob[:open], glob[close+1:]
	var out []string
	start := open + 1
	for _, end := range append(commas, close) {
		more, err := expandBraces(prefix + glob[start:end] + suffix)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
		if len(out) > maxAlternatives {
			return nil, errors.New("too many {,} alternatives")
		}
		start = end + 1
	}
	return out, nil
}

// findBraces locates the first top-level "{", its matching "}", and the
// top-level commas between them. open is -1 when there is no group.
func findBraces(glob string) (open, close int, commas []int, err error) {
	open, depth := -1, 0
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			i++
		case '[':
			i = skipClass(glob, i)
		case '{':
			if depth == 0 {
				open = i
			}
			depth++
		case ',':
			if depth == 1 {
				commas = append(commas, i)
			}
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				return open, i, commas, nil
			}
		}
	}
	if depth > 0 {
		return 0, 0, nil, errors.New("unclosed {")
	}
	return -1, 0, nil, nil
}

// skipClass returns the index of the "]" closing the class opened at i, or
// len(glob) when it is unclosed (path.Match will then reject the pattern).
func skipClass(glob string, i int) int {
	i++
	if i < len(glob) && glob[i] == '^' {
		i++
	}
	if i < len(glob) && glob[i] == ']' {
		i++
	}
	for ; i < len(glob); i++ {
		switch glob[i] {
		case '\\':
			i++
		case ']':
			return i
		}
	}
	return len(glob)
}
