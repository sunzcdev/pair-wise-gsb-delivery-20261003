package incremental

import (
	"fmt"
	"strconv"
	"strings"
)

// Path locates a value inside the environment. Segments are either strings
// (struct fields and map keys) or ints (array and slice indices). For
// example, the path of `items[2].Price` is Path{"items", 2, "Price"}.
type Path []any

// ParsePath parses a path expression like "items[2].Price" or `a["b"][0]`
// into a Path.
func ParsePath(s string) (Path, error) {
	var p Path
	i := 0
	readIdent := func() (string, error) {
		start := i
		for i < len(s) && s[i] != '.' && s[i] != '[' {
			i++
		}
		if i == start {
			return "", fmt.Errorf("invalid path %q: empty segment", s)
		}
		return s[start:i], nil
	}
	for i < len(s) {
		switch s[i] {
		case '.':
			i++
			seg, err := readIdent()
			if err != nil {
				return nil, err
			}
			p = append(p, seg)
		case '[':
			i++
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				quote := s[i]
				i++
				start := i
				for i < len(s) && s[i] != quote {
					i++
				}
				if i >= len(s) {
					return nil, fmt.Errorf("invalid path %q: unterminated string", s)
				}
				seg := s[start:i]
				i++
				if i >= len(s) || s[i] != ']' {
					return nil, fmt.Errorf("invalid path %q: expected ]", s)
				}
				i++
				p = append(p, seg)
			} else {
				start := i
				for i < len(s) && s[i] != ']' {
					i++
				}
				if i >= len(s) {
					return nil, fmt.Errorf("invalid path %q: expected ]", s)
				}
				n, err := strconv.Atoi(s[start:i])
				if err != nil {
					return nil, fmt.Errorf("invalid path %q: %v", s, err)
				}
				i++
				p = append(p, n)
			}
		default:
			seg, err := readIdent()
			if err != nil {
				return nil, err
			}
			p = append(p, seg)
		}
	}
	if len(p) == 0 {
		return nil, fmt.Errorf("invalid path %q: empty path", s)
	}
	return p, nil
}

// String returns a human-readable form of the path, e.g. "items[2].Price".
func (p Path) String() string {
	var b strings.Builder
	for i, seg := range p {
		switch v := seg.(type) {
		case string:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		case int:
			fmt.Fprintf(&b, "[%d]", v)
		default:
			fmt.Fprintf(&b, "[%v]", v)
		}
	}
	return b.String()
}

func (p Path) equal(q Path) bool {
	if len(p) != len(q) {
		return false
	}
	for i := range p {
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

func (p Path) hasPrefix(q Path) bool {
	if len(q) > len(p) {
		return false
	}
	return p[:len(q)].equal(q)
}

// dep is a recorded environment read. A deep dependency is invalidated by
// changes at the path itself, below it, or above it (the read value may be
// used arbitrarily). A shallow dependency is only invalidated when the
// path itself or one of its ancestors is replaced; it is used for values
// that are only traversed (for example the array argument of filter, whose
// element reads are tracked individually).
type dep struct {
	path Path
	deep bool
}

// affectedBy reports whether a cached value depending on d may change when
// the value at change is replaced.
func (d dep) affectedBy(change Path) bool {
	if d.deep {
		return d.path.hasPrefix(change) || change.hasPrefix(d.path)
	}
	return d.path.hasPrefix(change)
}

func appendPath(p Path, seg any) Path {
	out := make(Path, len(p)+1)
	copy(out, p)
	out[len(p)] = seg
	return out
}
