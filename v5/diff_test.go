package jsonpatch

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/evanphx/json-patch/v5/internal/json"
)

func applyDiff(t *testing.T, original, modified string, opts *DiffOptions) Patch {
	t.Helper()

	patch, err := CreatePatchWithOptions([]byte(original), []byte(modified), opts)
	if err != nil {
		t.Fatalf("CreatePatchWithOptions failed: %v", err)
	}

	patched, err := patch.Apply([]byte(original))
	if err != nil {
		patchJSON, _ := json.Marshal(patch)
		t.Fatalf("applying generated patch failed: %v\npatch: %s", err, patchJSON)
	}

	if !Equal(patched, []byte(modified)) {
		patchJSON, _ := json.Marshal(patch)
		t.Fatalf("patched document does not match modified document\npatch:   %s\npatched: %s\nmodified:%s", patchJSON, patched, modified)
	}

	return patch
}

func TestCreatePatchScalarsAndObjects(t *testing.T) {
	original := `{"a": 1, "b": {"c": "x", "d": [1, 2, 3]}, "gone": true}`
	modified := `{"a": 2, "b": {"c": "x", "d": [1, 2, 4, 5]}, "new": "yes"}`

	patch := applyDiff(t, original, modified, nil)

	var kinds []string
	for _, op := range patch {
		kinds = append(kinds, op.Kind())
	}
	want := []string{"remove", "replace", "replace", "add", "add"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected op kinds: got %v, want %v", kinds, want)
	}
}

func TestCreatePatchIdenticalDocuments(t *testing.T) {
	doc := `{"a": [1, 2, {"b": "c"}], "d": null}`
	patch, err := CreatePatch([]byte(doc), []byte(doc))
	if err != nil {
		t.Fatalf("CreatePatch failed: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty patch, got %v", patch)
	}
}

func TestCreatePatchKeyedArrayReorderAndUpdate(t *testing.T) {
	original := `{"servers": [
		{"id": "web-1", "cpu": 4},
		{"id": "web-2", "cpu": 4},
		{"id": "db-1", "cpu": 8},
		{"id": "cache-1", "cpu": 2}
	]}`
	modified := `{"servers": [
		{"id": "db-1", "cpu": 8},
		{"id": "web-2", "cpu": 16},
		{"id": "web-1", "cpu": 4},
		{"id": "web-3", "cpu": 4}
	]}`

	opts := &DiffOptions{ArrayIdentityKeys: map[string]string{"/servers": "id"}}
	patch := applyDiff(t, original, modified, opts)

	var kinds []string
	for _, op := range patch {
		kinds = append(kinds, op.Kind())
	}
	want := []string{"remove", "move", "move", "add", "replace"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected op kinds: got %v, want %v", kinds, want)
	}

	move, err := patch[1].From()
	if err != nil {
		t.Fatalf("move op missing from: %v", err)
	}
	if move != "/servers/2" {
		t.Fatalf("unexpected move source: %s", move)
	}
}

func TestCreatePatchKeyedArrayNested(t *testing.T) {
	original := `{"spec": {"containers": [{"name": "a", "image": "a:1"}, {"name": "b", "image": "b:1"}]}}`
	modified := `{"spec": {"containers": [{"name": "b", "image": "b:2"}, {"name": "a", "image": "a:1"}]}}`

	opts := &DiffOptions{ArrayIdentityKeys: map[string]string{"/spec/containers": "name"}}
	applyDiff(t, original, modified, opts)
}

func TestCreatePatchDuplicateIdentity(t *testing.T) {
	original := `{"servers": [{"id": "a"}, {"id": "a"}]}`
	modified := `{"servers": [{"id": "a"}, {"id": "a"}, {"id": "b"}]}`

	opts := &DiffOptions{ArrayIdentityKeys: map[string]string{"/servers": "id"}}
	_, err := CreatePatchWithOptions([]byte(original), []byte(modified), opts)
	if !errors.Is(err, ErrDuplicateIdentity) {
		t.Fatalf("expected ErrDuplicateIdentity, got %v", err)
	}

	// With the fallback policy the same input deterministically produces a
	// valid index-based patch instead of an error.
	opts.OnIdentityMismatch = IdentityMismatchFallback
	patch := applyDiff(t, original, modified, opts)
	if len(patch) != 1 || patch[0].Kind() != "add" {
		t.Fatalf("unexpected fallback patch: %v", patch)
	}
}

func TestCreatePatchMissingIdentity(t *testing.T) {
	original := `{"servers": [{"id": "a"}, {"name": "no-id"}]}`
	modified := `{"servers": [{"id": "a"}, {"id": "b"}]}`

	opts := &DiffOptions{ArrayIdentityKeys: map[string]string{"/servers": "id"}}
	_, err := CreatePatchWithOptions([]byte(original), []byte(modified), opts)
	if !errors.Is(err, ErrMissingIdentity) {
		t.Fatalf("expected ErrMissingIdentity, got %v", err)
	}

	opts.OnIdentityMismatch = IdentityMismatchFallback
	applyDiff(t, original, modified, opts)
}

func TestCreatePatchNumericIdentities(t *testing.T) {
	original := `{"rules": [{"prio": 1, "v": "a"}, {"prio": 2, "v": "b"}]}`
	modified := `{"rules": [{"prio": 2, "v": "b"}, {"prio": 1, "v": "a"}]}`

	opts := &DiffOptions{ArrayIdentityKeys: map[string]string{"/rules": "prio"}}
	patch := applyDiff(t, original, modified, opts)
	if len(patch) != 1 || patch[0].Kind() != "move" {
		t.Fatalf("expected a single move, got %v", patch)
	}
}

func TestCreatePatchEscapedKeys(t *testing.T) {
	original := `{"a/b": {"~x": 1}}`
	modified := `{"a/b": {"~x": 2}}`

	patch := applyDiff(t, original, modified, nil)
	path, err := patch[0].Path()
	if err != nil {
		t.Fatalf("missing path: %v", err)
	}
	if path != "/a~1b/~0x" {
		t.Fatalf("path not RFC 6901 escaped: %s", path)
	}
}

func ExampleCreatePatchWithOptions() {
	original := []byte(`{
		"cluster": "prod",
		"servers": [
			{"id": "web-1", "cpu": 4, "enabled": true},
			{"id": "web-2", "cpu": 4, "enabled": true},
			{"id": "db-1", "cpu": 8, "enabled": true},
			{"id": "cache-1", "cpu": 2, "enabled": false}
		]
	}`)
	modified := []byte(`{
		"cluster": "prod",
		"servers": [
			{"id": "db-1", "cpu": 8, "enabled": true},
			{"id": "web-2", "cpu": 16, "enabled": true},
			{"id": "web-1", "cpu": 4, "enabled": true},
			{"id": "web-3", "cpu": 4, "enabled": true}
		]
	}`)

	opts := &DiffOptions{
		ArrayIdentityKeys: map[string]string{"/servers": "id"},
	}
	patch, err := CreatePatchWithOptions(original, modified, opts)
	if err != nil {
		panic(err)
	}

	for _, op := range patch {
		buf, _ := json.Marshal(op)
		fmt.Println(string(buf))
	}

	// Output:
	// {"op":"remove","path":"/servers/3"}
	// {"from":"/servers/2","op":"move","path":"/servers/0"}
	// {"from":"/servers/2","op":"move","path":"/servers/1"}
	// {"op":"add","path":"/servers/3","value":{"cpu":4,"enabled":true,"id":"web-3"}}
	// {"op":"replace","path":"/servers/1/cpu","value":16}
}

func benchmarkKeyedReorder(b *testing.B, n int) {
	original := make([]interface{}, n)
	for i := 0; i < n; i++ {
		original[i] = map[string]interface{}{
			"id":      fmt.Sprintf("svc-%06d", i),
			"cpu":     json.Number("4"),
			"memory":  "8Gi",
			"enabled": true,
		}
	}

	// Fully reshuffle the elements and touch every 10th element's content.
	perm := rand.New(rand.NewSource(42)).Perm(n)
	modified := make([]interface{}, n)
	for j, i := range perm {
		elem := original[i].(map[string]interface{})
		if j%10 == 0 {
			copyElem := make(map[string]interface{}, len(elem))
			for k, v := range elem {
				copyElem[k] = v
			}
			copyElem["cpu"] = json.Number("8")
			elem = copyElem
		}
		modified[j] = elem
	}

	originalJSON, err := json.Marshal(map[string]interface{}{"servers": original})
	if err != nil {
		b.Fatal(err)
	}
	modifiedJSON, err := json.Marshal(map[string]interface{}{"servers": modified})
	if err != nil {
		b.Fatal(err)
	}

	opts := &DiffOptions{ArrayIdentityKeys: map[string]string{"/servers": "id"}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CreatePatchWithOptions(originalJSON, modifiedJSON, opts); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCreatePatchKeyedReorder100(b *testing.B)   { benchmarkKeyedReorder(b, 100) }
func BenchmarkCreatePatchKeyedReorder1000(b *testing.B)  { benchmarkKeyedReorder(b, 1000) }
func BenchmarkCreatePatchKeyedReorder10000(b *testing.B) { benchmarkKeyedReorder(b, 10000) }
