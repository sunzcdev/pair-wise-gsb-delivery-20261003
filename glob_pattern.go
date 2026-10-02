package sftp

import (
	"path"
	"strings"
)

type globSegment struct {
	part string
	glob bool // contains ordinary glob metacharacters
	star bool // exactly "**", matches zero or more segments
}

// A compiled glob is the fixed anchor directory (slash separated) and the
// remaining pattern segments relative to the anchor (the anchor's own
// segments are not included).
type globPattern struct {
	anchor   string
	segments []globSegment
	literal  bool
}

// compileGlobPattern splits pattern into slash separated segments and marks
// globstars. literal is true when no segment contains metacharacters.
// A malformed segment yields ErrBadPattern.
func compileGlobPattern(pattern string) (globPattern, error) {
	cleaned := path.Clean(pattern)
	if cleaned == "." || cleaned == "/" {
		return globPattern{anchor: cleaned, literal: true}, nil
	}
	rooted := strings.HasPrefix(cleaned, "/")
	parts := strings.Split(cleaned, "/")
	segments := make([]globSegment, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if isGlobStar(part) {
			if len(segments) > 0 && segments[len(segments)-1].star {
				continue
			}
			segments = append(segments, globSegment{part: "**", star: true})
			continue
		}
		if hasMeta(part) {
			if _, err := path.Match(part, ""); err != nil {
				return globPattern{}, err
			}
			segments = append(segments, globSegment{part: part, glob: true})
			continue
		}
		segments = append(segments, globSegment{part: part})
	}

	i := 0
	for i < len(segments) && !segments[i].glob && !segments[i].star {
		i++
	}
	p := globPattern{segments: segments}
	if i == 0 {
		if rooted {
			p.anchor = "/"
		} else {
			p.anchor = "."
		}
	} else {
		fixed := make([]string, i)
		for j := 0; j < i; j++ {
			fixed[j] = segments[j].part
		}
		p.anchor = strings.Join(fixed, "/")
		if rooted {
			p.anchor = "/" + p.anchor
		}
	}
	if i < len(segments) {
		p.literal = false
		p.segments = segments[i:]
		return p, nil
	}
	// Fully literal: the anchor already names the (single) target.
	p.literal = true
	p.anchor = cleaned // path.Clean preserves the leading slash
	p.segments = nil
	return p, nil
}

// isGlobStar reports whether part consists only of '*' and is at least "**".
func isGlobStar(part string) bool {
	if len(part) < 2 {
		return false
	}
	for i := 0; i < len(part); i++ {
		if part[i] != '*' {
			return false
		}
	}
	return true
}
