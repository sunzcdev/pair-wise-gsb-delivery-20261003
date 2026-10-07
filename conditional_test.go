package webdav

import (
	"errors"
	"net/http"
	"testing"
)

func TestParseConditionalMatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		set      bool
		wildcard bool
		tags     []ETag
		wantErr  bool
	}{
		{name: "empty", header: "", set: false},
		{name: "wildcard", header: "*", set: true, wildcard: true},
		{name: "wildcard padded", header: "  * ", set: true, wildcard: true},
		{name: "single strong", header: `"v1"`, set: true, tags: []ETag{{Opaque: "v1"}}},
		{name: "single weak", header: `W/"v1"`, set: true, tags: []ETag{{Opaque: "v1", Weak: true}}},
		{name: "list", header: `"v1", "v2", W/"v3"`, set: true, tags: []ETag{
			{Opaque: "v1"},
			{Opaque: "v2"},
			{Opaque: "v3", Weak: true},
		}},
		{name: "list no spaces", header: `"v1","v2"`, set: true, tags: []ETag{{Opaque: "v1"}, {Opaque: "v2"}}},
		{name: "escaped quote", header: `"a\"b"`, set: true, tags: []ETag{{Opaque: `a\"b`}}},
		{name: "bare wildcard not tag", header: `v1`, wantErr: true},
		{name: "unterminated quote", header: `"v1`, wantErr: true},
		{name: "wildcard mixed", header: `*, "v1"`, wantErr: true},
		{name: "weak must quote", header: `W/v1`, wantErr: true},
		{name: "trailing comma", header: `"v1",`, wantErr: true},
		{name: "empty tag", header: `""`, set: true, tags: []ETag{{Opaque: ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseConditionalMatch(tc.header)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.IsSet() != tc.set {
				t.Fatalf("IsSet: got %v want %v", got.IsSet(), tc.set)
			}
			if got.IsWildcard() != tc.wildcard {
				t.Fatalf("IsWildcard: got %v want %v", got.IsWildcard(), tc.wildcard)
			}
			if len(got.Tags) != len(tc.tags) {
				t.Fatalf("tags: got %+v want %+v", got.Tags, tc.tags)
			}
			for i := range tc.tags {
				if got.Tags[i] != tc.tags[i] {
					t.Fatalf("tag %d: got %+v want %+v", i, got.Tags[i], tc.tags[i])
				}
			}
			if got.IsSet() && got.String() == "" {
				t.Fatalf("String() empty for %q", tc.header)
			}
		})
	}
}

func TestETagCompare(t *testing.T) {
	strong := ETag{Opaque: "v1"}
	weak := ETag{Opaque: "v1", Weak: true}
	other := ETag{Opaque: "v2"}

	if StrongCompare(strong, strong) != true {
		t.Error("strong/strong same value should strongly compare")
	}
	if StrongCompare(strong, weak) {
		t.Error("strong vs weak must not strongly compare")
	}
	if StrongCompare(weak, weak) {
		t.Error("weak vs weak must not strongly compare")
	}
	if StrongCompare(strong, other) {
		t.Error("different values must not strongly compare")
	}
	if !WeakCompare(strong, weak) {
		t.Error("strong vs weak same value should weakly compare")
	}
	if !WeakCompare(weak, weak) {
		t.Error("weak vs weak same value should weakly compare")
	}
	if WeakCompare(strong, other) {
		t.Error("different values must not weakly compare")
	}
}

func TestIfMatchSemantics(t *testing.T) {
	strong := ETag{Opaque: "v1"}
	weakCurrent := ETag{Opaque: "v1", Weak: true}

	// "*" matches only when the resource exists.
	wild := NewWildcardMatch()
	if !wild.IfMatchMatch(true, strong) {
		t.Error("* should match existing resource")
	}
	if wild.IfMatchMatch(false, ETag{}) {
		t.Error("* should not match missing resource")
	}

	// Strong list matches the strong current tag.
	list := NewConditionalMatch(ETag{Opaque: "v0"}, strong)
	if !list.IfMatchMatch(true, strong) {
		t.Error("list should match current strong tag")
	}
	// A weak tag in the list must not strongly match, even on equal opaque.
	weakList := NewConditionalMatch(weakCurrent)
	if weakList.IfMatchMatch(true, strong) {
		t.Error("weak tag in If-Match list must not strongly match strong resource")
	}
	// A strong listed tag must not match a weak current tag.
	if list.IfMatchMatch(true, weakCurrent) {
		t.Error("strong listed tag must not match weak current tag")
	}
	// Missing resource never matches a tag list.
	if list.IfMatchMatch(false, ETag{}) {
		t.Error("tag list should not match missing resource")
	}
}

func TestIfNoneMatchSemantics(t *testing.T) {
	strong := ETag{Opaque: "v1"}
	weak := ETag{Opaque: "v1", Weak: true}

	// "*" matches when resource exists (precondition fails for create).
	wild := NewWildcardMatch()
	if !wild.IfNoneMatchMatch(true, strong) {
		t.Error("* If-None-Match should match existing resource")
	}
	if wild.IfNoneMatchMatch(false, ETag{}) {
		t.Error("* If-None-Match should not match missing resource")
	}

	// Weak comparison: weak listed tag matches strong current with equal opaque.
	weakList := NewConditionalMatch(weak)
	if !weakList.IfNoneMatchMatch(true, strong) {
		t.Error("weak listed tag should weakly match strong current tag")
	}
	if !weakList.IfNoneMatchMatch(true, weak) {
		t.Error("weak listed tag should weakly match weak current tag")
	}
	unrelated := NewConditionalMatch(ETag{Opaque: "other"})
	if unrelated.IfNoneMatchMatch(true, strong) {
		t.Error("unrelated tag should not weak-match")
	}
}

func TestCheckConditionsAtomic(t *testing.T) {
	current := ETag{Opaque: "v1"}

	// If-Match success.
	err := CheckConditionsAtomic(Conditions{IfMatch: NewConditionalMatch(current)}, true, current)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	// If-Match stale -> 412.
	err = CheckConditionsAtomic(Conditions{IfMatch: NewConditionalMatch(ETag{Opaque: "old"})}, true, current)
	if statusOf(err) != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %v", err)
	}

	// If-None-Match * on missing -> success.
	err = CheckConditionsAtomic(Conditions{IfNoneMatch: NewWildcardMatch()}, false, ETag{})
	if err != nil {
		t.Fatalf("expected create success, got %v", err)
	}

	// If-None-Match * on existing -> 412.
	err = CheckConditionsAtomic(Conditions{IfNoneMatch: NewWildcardMatch()}, true, current)
	if statusOf(err) != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %v", err)
	}

	// No conditions -> always fine.
	if err := CheckConditionsAtomic(Conditions{}, true, current); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckConditionsUnsupported(t *testing.T) {
	err := CheckConditions(Conditions{IfMatch: NewWildcardMatch()}, true, ETag{})
	if statusOf(err) != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %v", err)
	}
	if !errors.Is(err, ErrConditionsNotSupported) {
		t.Fatalf("expected ErrConditionsNotSupported, got %v", err)
	}
	if !IsConditionsNotSupported(err) {
		t.Fatal("IsConditionsNotSupported should be true")
	}
	// Unset conditions are fine even for an incapable backend.
	if err := CheckConditions(Conditions{}, false, ETag{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
func statusOf(err error) int {
	if he, ok := err.(*HTTPError); ok {
		return he.Code
	}
	return 0
}
