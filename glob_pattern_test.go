package sftp

import (
	"testing"
)

func TestCompileGlobPattern(t *testing.T) {
	tests := []struct {
		pattern string
		anchor  string
		literal bool
		segs    []string
		err     bool
	}{
		{"logs/**/error-*.gz", "logs", false, []string{"**", "error-*.gz"}, false},
		{"a/**/**/b", "a", false, []string{"**", "b"}, false},
		{"**/x", ".", false, []string{"**", "x"}, false},
		{"/var/log/**/*.gz", "/var/log", false, []string{"**", "*.gz"}, false},
		{"logs/*/error.gz", "logs", false, []string{"*", "error.gz"}, false},
		{"logs/error.gz", "logs/error.gz", true, nil, false},
		{"**", ".", false, []string{"**"}, false},
		{"/**", "/", false, []string{"**"}, false},
		{"/var/log/x.gz", "/var/log/x.gz", true, nil, false},
		{"logs/[", "logs", false, nil, true},
	}

	for _, tt := range tests {
		p, err := compileGlobPattern(tt.pattern)
		if tt.err {
			if err == nil {
				t.Errorf("compile(%q): expected error, got nil", tt.pattern)
			}
			continue
		}
		if err != nil {
			t.Errorf("compile(%q): unexpected error %v", tt.pattern, err)
			continue
		}
		if p.anchor != tt.anchor {
			t.Errorf("compile(%q).anchor = %q, want %q", tt.pattern, p.anchor, tt.anchor)
		}
		if p.literal != tt.literal {
			t.Errorf("compile(%q).literal = %v, want %v", tt.pattern, p.literal, tt.literal)
		}
		var got []string
		for _, s := range p.segments {
			got = append(got, s.part)
		}
		if len(got) != len(tt.segs) {
			t.Errorf("compile(%q).segs = %v, want %v", tt.pattern, got, tt.segs)
			continue
		}
		for i := range got {
			if got[i] != tt.segs[i] {
				t.Errorf("compile(%q).segs = %v, want %v", tt.pattern, got, tt.segs)
			}
		}
	}
}

func TestGlobStarMatching(t *testing.T) {
	// The globstar segment only matches whole names; ordinary glob rules
	// still apply to non-"**" segments.
	if _, err := Match("error-*.gz", "error-1.gz"); err != nil {
		t.Fatal(err)
	}
	ok, _ := Match("error-*.gz", "nope")
	if ok {
		t.Fatal("expected no match")
	}
	if !isGlobStar("**") || !isGlobStar("***") {
		t.Fatal("** and *** must be globstars")
	}
	if isGlobStar("*") || isGlobStar("a**") {
		t.Fatal("single star and mixed names are not globstars")
	}
}
