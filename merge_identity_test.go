package aconfig

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

type mergePool struct {
	Name string            `json:"name" yaml:"name" toml:"name" hcl:"name"`
	Host string            `json:"host" yaml:"host" toml:"host" hcl:"host"`
	Port int               `json:"port" yaml:"port" toml:"port" hcl:"port"`
	Meta map[string]string `json:"meta" yaml:"meta" toml:"meta" hcl:"meta"`
}

type mergeConfig struct {
	Pools    []mergePool `json:"pools" merge:"name"`
	Hosts    []string    `json:"hosts"`
	Empty    []mergePool `json:"empty" merge:"name"`
	PlainObj []mergePool `json:"plain_obj"`
}

type mergeEndpoint struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type mergeService struct {
	Name      string          `json:"name"`
	Endpoints []mergeEndpoint `json:"endpoints" merge:"id"`
}

type mergeNestedConfig struct {
	Services []mergeService `json:"services" merge:"name"`
}

func mergeLoader(t *testing.T, cfg any, files map[string]string) *Loader {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return LoaderFor(cfg, Config{
		NewParser:    true,
		SkipDefaults: true,
		SkipEnv:      true,
		SkipFlags:    true,
		MergeFiles:   true,
		FileSystem:   fsys,
		Files:        keysSorted(files),
	})
}

func keysSorted(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	// keep the deterministic base/over1/over2 ordering used in the tests
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

func TestIdentityMerge_UpdateKeepAppend(t *testing.T) {
	files := map[string]string{
		"a_base.json":  `{"pools":[{"name":"a","host":"h1","port":1},{"name":"b","host":"h2","port":2}]}`,
		"b_over.json":  `{"pools":[{"name":"a","port":99},{"name":"c","host":"h3","port":3}]}`,
		"c_over2.json": `{"pools":[{"name":"d","host":"h4","port":4}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}

	want := []mergePool{
		{Name: "a", Host: "h1", Port: 99},
		{Name: "b", Host: "h2", Port: 2},
		{Name: "c", Host: "h3", Port: 3},
		{Name: "d", Host: "h4", Port: 4},
	}
	if got := cfg.Pools; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestIdentityMerge_Delete(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","host":"h1","port":1},{"name":"b","host":"h2","port":2},{"name":"c","host":"h3","port":3}]}`,
		"b_over.json": `{"pools":[{"name":"b","$delete":true},{"name":"a","port":7}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}

	want := []mergePool{
		{Name: "a", Host: "h1", Port: 7},
		{Name: "c", Host: "h3", Port: 3},
	}
	if got := cfg.Pools; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestIdentityMerge_DeleteAll(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","host":"h1","port":1},{"name":"b","host":"h2","port":2}]}`,
		"b_over.json": `{"pools":[{"name":"a","$delete":true},{"name":"b","$delete":true}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Pools) != 0 {
		t.Fatalf("want empty, got %v", cfg.Pools)
	}
}

func TestIdentityMerge_EmptyArrayKeepsBase(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","host":"h1","port":1}]}`,
		"b_over.json": `{"pools":[]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}

	want := []mergePool{{Name: "a", Host: "h1", Port: 1}}
	if got := cfg.Pools; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestIdentityMerge_PlainSliceReplaced(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"hosts":["one","two","three"]}`,
		"b_over.json": `{"hosts":["four"]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}
	want := []string{"four"}
	if got := cfg.Hosts; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestIdentityMerge_PlainObjectSliceReplaced(t *testing.T) {
	// A struct slice without an identity tag keeps the replacement rule.
	files := map[string]string{
		"a_base.json": `{"plain_obj":[{"name":"a","host":"h1","port":1}]}`,
		"b_over.json": `{"plain_obj":[{"name":"z","host":"hz","port":9}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}
	want := []mergePool{{Name: "z", Host: "hz", Port: 9}}
	if got := cfg.PlainObj; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestIdentityMerge_DuplicateInOverlay(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","host":"h1","port":1}]}`,
		"b_over.json": `{"pools":[{"name":"a","port":1},{"name":"a","port":2}]}`,
	}

	var cfg mergeConfig
	err := mergeLoader(t, &cfg, files).Load()
	if err == nil || !strings.Contains(err.Error(), "duplicate identity") {
		t.Fatalf("want duplicate identity error, got %v", err)
	}
}

func TestIdentityMerge_DuplicateInBase(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","port":1},{"name":"a","port":2}]}`,
		"b_over.json": `{"pools":[{"name":"a","port":3}]}`,
	}

	var cfg mergeConfig
	err := mergeLoader(t, &cfg, files).Load()
	if err == nil || !strings.Contains(err.Error(), "duplicate identity") {
		t.Fatalf("want duplicate identity error, got %v", err)
	}
}

func TestIdentityMerge_MissingIdentityInOverlay(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","port":1}]}`,
		"b_over.json": `{"pools":[{"host":"h","port":2}]}`,
	}

	var cfg mergeConfig
	err := mergeLoader(t, &cfg, files).Load()
	if err == nil || !strings.Contains(err.Error(), "missing identity field") {
		t.Fatalf("want missing identity error, got %v", err)
	}
}

func TestIdentityMerge_IdentityCannotChange(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","port":1}]}`,
		"b_over.json": `{"pools":[{"name":"a","port":2},{"name":"a","port":3}]}`,
	}

	var cfg mergeConfig
	err := mergeLoader(t, &cfg, files).Load()
	if err == nil || !strings.Contains(err.Error(), "duplicate identity") {
		t.Fatalf("want duplicate identity error, got %v", err)
	}
}

func TestIdentityMerge_NestedMapField(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","host":"h1","port":1,"meta":{"region":"cn","tier":"gold"}}]}`,
		"b_over.json": `{"pools":[{"name":"a","meta":{"tier":"silver"}}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}
	want := []mergePool{{
		Name: "a", Host: "h1", Port: 1,
		Meta: map[string]string{"region": "cn", "tier": "silver"},
	}}
	if got := cfg.Pools; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestIdentityMerge_NestedIdentitySlice(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"services":[
			{"name":"svc","endpoints":[{"id":"e1","addr":"1.1.1.1"},{"id":"e2","addr":"2.2.2.2"}]}
		]}`,
		"b_over.json": `{"services":[
			{"name":"svc","endpoints":[{"id":"e1","addr":"9.9.9.9"},{"id":"e3","addr":"3.3.3.3"}]}
		]}`,
	}

	var cfg mergeNestedConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}

	want := mergeNestedConfig{
		Services: []mergeService{{
			Name: "svc",
			Endpoints: []mergeEndpoint{
				{ID: "e1", Addr: "9.9.9.9"},
				{ID: "e2", Addr: "2.2.2.2"},
				{ID: "e3", Addr: "3.3.3.3"},
			},
		}},
	}
	if got := cfg; !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %+v\nwant %+v", got, want)
	}
}

func TestIdentityMerge_NestedIdentityCannotChange(t *testing.T) {
	endpointSchema := schemaFor(reflect.TypeOf(mergeEndpoint{}))
	endpointSchema.identityKeys = []string{"id"}

	base := map[string]any{"id": "e1", "addr": "1.1.1.1"}
	overlay := map[string]any{"id": "e9", "addr": "9.9.9.9"}

	_, err := mergeObject(base, overlay, endpointSchema, "endpoints[0]")
	if err == nil || !strings.Contains(err.Error(), "identity field") {
		t.Fatalf("want identity field error, got %v", err)
	}

	// Base must remain untouched.
	if base["id"] != "e1" {
		t.Fatalf("base object mutated: %v", base)
	}
}

func TestIdentityMerge_DoesNotMutateDecodedObjects(t *testing.T) {
	files := map[string]string{
		"a_base.json": `{"pools":[{"name":"a","host":"h1","port":1}]}`,
		"b_over.json": `{"pools":[{"name":"a","port":42}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}

	// Loading a second time with a fresh destination must not observe state
	// leaking through shared maps/slices from previous decoded objects.
	var cfg2 mergeConfig
	if err := mergeLoader(t, &cfg2, files).Load(); err != nil {
		t.Fatal(err)
	}
	if got := cfg2.Pools; !reflect.DeepEqual(got, cfg.Pools) {
		t.Fatalf("second load differs:\n got %v\nwant %v", got, cfg.Pools)
	}
}

func TestMergeValues_DoesNotMutateInputs(t *testing.T) {
	poolSchema := fieldSchemaFor(reflect.TypeOf(mergeConfig{}).Field(0))

	base := []any{
		map[string]any{"name": "a", "host": "h1", "port": float64(1)},
	}
	overlay := []any{
		map[string]any{"name": "a", "port": float64(9)},
		map[string]any{"name": "c", "host": "h3", "port": float64(3)},
	}

	baseSnap := fmt.Sprint(base)
	overSnap := fmt.Sprint(overlay)

	if _, err := mergeValues(base, overlay, poolSchema, "pools"); err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(base) != baseSnap {
		t.Fatalf("base mutated:\n got %v\nwant %v", base, baseSnap)
	}
	if fmt.Sprint(overlay) != overSnap {
		t.Fatalf("overlay mutated:\n got %v\nwant %v", overlay, overSnap)
	}

	// Nested map values must be copied, not shared.
	res, err := mergeValues(
		map[string]any{"name": "a", "meta": map[string]any{"k": "v"}},
		map[string]any{"meta": map[string]any{"k2": "v2"}},
		poolSchema.elem, "pools[0]",
	)
	if err != nil {
		t.Fatal(err)
	}
	res.(map[string]any)["meta"].(map[string]any)["k3"] = "v3"
	if _, leaked := base[0].(map[string]any)["k3"]; leaked {
		t.Fatal("nested map aliased into base")
	}
}

func TestMergeValues_NormalizesMapAnyAny(t *testing.T) {
	poolSchema := fieldSchemaFor(reflect.TypeOf(mergeConfig{}).Field(0))

	base := []any{
		map[any]any{"name": "a", "host": "h1", "port": 1},
	}
	overlay := []any{
		map[any]any{"name": "a", "port": 42},
	}
	res, err := mergeValues(base, overlay, poolSchema, "pools")
	if err != nil {
		t.Fatal(err)
	}
	items := res.([]any)
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("want map[string]any, got %T", items[0])
	}
	if first["host"] != "h1" || first["port"] != 42 {
		t.Fatalf("unexpected merged item: %v", first)
	}
}

func TestIdentityMerge_NewItemOrderAcrossLayers(t *testing.T) {
	files := map[string]string{
		"a_base.json":  `{"pools":[{"name":"a","port":1}]}`,
		"b_over.json":  `{"pools":[{"name":"b","port":2},{"name":"c","port":3}]}`,
		"c_over2.json": `{"pools":[{"name":"e","port":5},{"name":"d","port":4}]}`,
	}

	var cfg mergeConfig
	if err := mergeLoader(t, &cfg, files).Load(); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"a", "b", "c", "e", "d"}
	gotNames := make([]string, 0, len(cfg.Pools))
	for _, p := range cfg.Pools {
		gotNames = append(gotNames, p.Name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("\n got %v\nwant %v", gotNames, wantNames)
	}
}

func TestIdentityMerge_BadIdentityTagPanics(t *testing.T) {
	type C struct {
		Pools []mergePool `json:"pools" merge:"nope"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic for missing identity field")
		}
	}()
	_ = LoaderFor(&C{}, Config{NewParser: true})
}

func TestIdentityMerge_NonSliceIdentityTagPanics(t *testing.T) {
	type C struct {
		Field string `json:"field" merge:"x"`
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic for identity tag on non-slice")
		}
	}()
	_ = LoaderFor(&C{}, Config{NewParser: true})
}
