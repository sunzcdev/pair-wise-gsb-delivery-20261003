package aconfig

import (
	"reflect"
	"testing"
	"testing/fstest"
)

type poolConfig struct {
	ID      string `json:"id" identity:"true"`
	Name    string `json:"name"`
	MaxConn int    `json:"max_conn"`
	TLS     struct {
		Enabled bool   `json:"enabled"`
		Server  string `json:"server"`
	} `json:"tls"`
}

type poolRootConfig struct {
	Pools []poolConfig `json:"pools"`
	Hosts []string     `json:"hosts"`
}

func identityLoader(t *testing.T, paths []string, files map[string]string, merge bool) (*poolRootConfig, *Loader) {
	t.Helper()
	fsys := fstest.MapFS{}
	for path, content := range files {
		fsys[path] = &fstest.MapFile{Data: []byte(content)}
	}

	var cfg poolRootConfig
	loader := LoaderFor(&cfg, Config{
		NewParser:    false,
		SkipDefaults: true,
		SkipEnv:      true,
		SkipFlags:    true,
		MergeFiles:   merge,
		Files:        paths,
		FileSystem:   fsys,
	})
	return &cfg, loader
}

func TestIdentitySliceMerge(t *testing.T) {
	base := `{
		"pools": [
			{"id": "primary", "name": "main", "max_conn": 10, "tls": {"enabled": true, "server": "old.example"}},
			{"id": "replica", "name": "ro", "max_conn": 5, "tls": {"enabled": false, "server": "replica.example"}},
			{"id": "cache", "name": "cache-old", "max_conn": 2}
		],
		"hosts": ["a", "b"]
	}`
	override := `{
		"pools": [
			{"id": "primary", "max_conn": 20, "tls": {"server": "new.example"}},
			{"id": "cache", "$delete": true},
			{"id": "analytics", "name": "olap", "max_conn": 8, "tls": {"enabled": true, "server": "olap.example"}}
		],
		"hosts": ["only-me"]
	}`

	cfg, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfErr(t, loader.Load())

	primary := poolConfig{ID: "primary", Name: "main", MaxConn: 20}
	primary.TLS.Enabled = true
	primary.TLS.Server = "new.example"

	replica := poolConfig{ID: "replica", Name: "ro", MaxConn: 5}
	replica.TLS.Server = "replica.example"

	analytics := poolConfig{ID: "analytics", Name: "olap", MaxConn: 8}
	analytics.TLS.Enabled = true
	analytics.TLS.Server = "olap.example"

	want := poolRootConfig{
		Pools: []poolConfig{primary, replica, analytics},
		Hosts: []string{"only-me"},
	}
	mustEqual(t, *cfg, want)
}

func TestIdentitySliceMergeOrderAcrossLayers(t *testing.T) {
	layer1 := `{"pools": [
		{"id": "a", "name": "A"},
		{"id": "b", "name": "B"}
	]}`
	layer2 := `{"pools": [
		{"id": "c", "name": "C"},
		{"id": "a", "name": "A2"}
	]}`
	layer3 := `{"pools": [
		{"id": "d", "name": "D"},
		{"id": "e", "name": "E"}
	]}`

	cfg, loader := identityLoader(t,
		[]string{"1.json", "2.json", "3.json"},
		map[string]string{"1.json": layer1, "2.json": layer2, "3.json": layer3}, true)
	failIfErr(t, loader.Load())

	gotIDs := make([]string, len(cfg.Pools))
	for i, p := range cfg.Pools {
		gotIDs[i] = p.ID
	}
	wantIDs := []string{"a", "b", "c", "d", "e"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("order = %v, want %v", gotIDs, wantIDs)
	}
	if cfg.Pools[0].Name != "A2" {
		t.Fatalf("nested update not applied: %+v", cfg.Pools[0])
	}
}

func TestIdentitySliceEmptyArrayKeepsBase(t *testing.T) {
	base := `{"pools": [
		{"id": "a", "name": "A"},
		{"id": "b", "name": "B"}
	]}`
	override := `{"pools": []}`

	cfg, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfErr(t, loader.Load())

	if len(cfg.Pools) != 2 {
		t.Fatalf("empty slice must keep base, got %d pools", len(cfg.Pools))
	}
}

func TestIdentitySliceDeleteAllExplicitly(t *testing.T) {
	base := `{"pools": [
		{"id": "a", "name": "A"},
		{"id": "b", "name": "B"}
	]}`
	override := `{"pools": [
		{"id": "a", "$delete": true},
		{"id": "b", "$delete": true}
	]}`

	cfg, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfErr(t, loader.Load())

	if len(cfg.Pools) != 0 {
		t.Fatalf("explicit deletion must remove all, got %+v", cfg.Pools)
	}
}

func TestIdentitySliceDeleteUnknownFails(t *testing.T) {
	base := `{"pools": [{"id": "a", "name": "A"}]}`
	override := `{"pools": [{"id": "ghost", "$delete": true}]}`

	_, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfOk(t, loader.Load())
}

func TestIdentitySliceDuplicateInBaseFails(t *testing.T) {
	base := `{"pools": [
		{"id": "a", "name": "A"},
		{"id": "a", "name": "A2"}
	]}`

	_, loader := identityLoader(t,
		[]string{"base.json"},
		map[string]string{"base.json": base}, true)
	failIfOk(t, loader.Load())
}

func TestIdentitySliceDuplicateInLayerFails(t *testing.T) {
	base := `{"pools": [{"id": "a", "name": "A"}]}`
	override := `{"pools": [
		{"id": "b", "name": "B"},
		{"id": "b", "name": "B2"}
	]}`

	_, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfOk(t, loader.Load())
}

func TestIdentitySliceMissingIdentityFails(t *testing.T) {
	base := `{"pools": [{"id": "a", "name": "A"}]}`
	override := `{"pools": [{"name": "no-id"}]}`

	_, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfOk(t, loader.Load())
}

func TestIdentitySliceMultipleIdentityFieldsFails(t *testing.T) {
	type element struct {
		A string `json:"a" identity:"true"`
		B string `json:"b" identity:"true"`
	}
	type root struct {
		Items []element `json:"items"`
	}

	fsys := fstest.MapFS{
		"base.json": {Data: []byte(`{"items":[{"a":"1","b":"2"}]}`)},
	}
	var cfg root
	loader := LoaderFor(&cfg, Config{
		NewParser: false, SkipDefaults: true, SkipEnv: true, SkipFlags: true,
		Files: []string{"base.json"}, FileSystem: fsys,
	})
	failIfOk(t, loader.Load())
}

func TestIdentitySliceCannotChangeIdentity(t *testing.T) {
	type element struct {
		Key  string `json:"key" identity:"true"`
		Name string `json:"name"`
	}
	type root struct {
		Items []element `json:"items"`
	}

	load := func(override string) (root, error) {
		fsys := fstest.MapFS{
			"base.json":     {Data: []byte(`{"items":[{"key":"a","name":"A"}]}`)},
			"override.json": {Data: []byte(override)},
		}
		var cfg root
		loader := LoaderFor(&cfg, Config{
			NewParser: false, SkipDefaults: true, SkipEnv: true, SkipFlags: true,
			MergeFiles: true, Files: []string{"base.json", "override.json"}, FileSystem: fsys,
		})
		err := loader.Load()
		return cfg, err
	}

	// Same identity with updated fields merges into the existing element.
	okCfg, err := load(`{"items":[{"key":"a","name":"B"}]}`)
	failIfErr(t, err)
	if okCfg.Items[0].Key != "a" || okCfg.Items[0].Name != "B" {
		t.Fatalf("unexpected merge result: %+v", okCfg.Items[0])
	}

	// A different identity never renames the element: it is appended and the
	// original element stays untouched.
	appendedCfg, err := load(`{"items":[{"key":"b","name":"B"}]}`)
	failIfErr(t, err)
	if len(appendedCfg.Items) != 2 || appendedCfg.Items[0].Key != "a" || appendedCfg.Items[0].Name != "A" {
		t.Fatalf("update must not rename an existing element: %+v", appendedCfg.Items)
	}
}

func TestIdentitySliceDoesNotMutateDecoderOutput(t *testing.T) {
	base := `{"pools": [
		{"id": "a", "name": "A"},
		{"id": "b", "name": "B"}
	]}`
	override := `{"pools": [
		{"id": "a", "name": "A2"},
		{"id": "b", "$delete": true}
	]}`

	cfg, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfErr(t, loader.Load())

	if len(cfg.Pools) != 1 || cfg.Pools[0].Name != "A2" {
		t.Fatalf("unexpected result: %+v", cfg.Pools)
	}

	dec := &jsonDecoder{}
	dec.Init(loader.config.FileSystem)
	raw, err := dec.DecodeFile("override.json")
	failIfErr(t, err)

	pools := raw["pools"].([]any)
	updated := pools[0].(map[string]any)
	deleted := pools[1].(map[string]any)
	if updated["name"] != "A2" {
		t.Fatalf("decoder output was mutated: %+v", updated)
	}
	if deleted[deleteKey] != true {
		t.Fatalf("delete marker was removed from the decoder output: %+v", deleted)
	}
}

func TestIdentitySliceDeleteThenReaddInLayer(t *testing.T) {
	base := `{"pools": [
		{"id": "a", "name": "A"},
		{"id": "b", "name": "B"}
	]}`
	override := `{"pools": [
		{"id": "b", "$delete": true},
		{"id": "b", "name": "B-new"}
	]}`

	cfg, loader := identityLoader(t,
		[]string{"base.json", "override.json"},
		map[string]string{"base.json": base, "override.json": override}, true)
	failIfErr(t, loader.Load())

	gotIDs := make([]string, len(cfg.Pools))
	for i, p := range cfg.Pools {
		gotIDs[i] = p.ID
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("order = %v, want %v", gotIDs, want)
	}
	if cfg.Pools[1].Name != "B-new" {
		t.Fatalf("re-added element should use fresh values: %+v", cfg.Pools[1])
	}
}

func TestIdentitySliceWithPointerElements(t *testing.T) {
	type element struct {
		ID   string `json:"id" identity:"true"`
		Port int    `json:"port"`
	}
	type root struct {
		Items []*element `json:"items"`
	}

	fsys := fstest.MapFS{
		"base.json":     {Data: []byte(`{"items":[{"id":"a","port":1}]}`)},
		"override.json": {Data: []byte(`{"items":[{"id":"a","port":2},{"id":"b","port":3}]}`)},
	}
	var cfg root
	loader := LoaderFor(&cfg, Config{
		NewParser: false, SkipDefaults: true, SkipEnv: true, SkipFlags: true,
		MergeFiles: true, Files: []string{"base.json", "override.json"}, FileSystem: fsys,
	})
	failIfErr(t, loader.Load())

	if len(cfg.Items) != 2 {
		t.Fatalf("want 2 items, got %+v", cfg.Items)
	}
	if cfg.Items[0].ID != "a" || cfg.Items[0].Port != 2 {
		t.Fatalf("unexpected updated item: %+v", cfg.Items[0])
	}
	if cfg.Items[1].ID != "b" || cfg.Items[1].Port != 3 {
		t.Fatalf("unexpected appended item: %+v", cfg.Items[1])
	}
}
