package extension_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuin/goldmark/v2/astjson"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
)

func extensionParser() parser.Parser {
	return parser.New(parser.WithExtensions(
		extension.NewStrikethroughParser(),
		extension.NewTableParser(),
		extension.NewTaskListItemParser(),
		extension.NewFootnoteParser(),
		extension.NewDefinitionListParser(),
	))
}

func exportExtensions(t *testing.T, source string, includeSource bool) []byte {
	t.Helper()
	doc := extensionParser().Parse([]byte(source))
	exp := extension.RegisterAdaptersOn(astjson.NewExporter(astjson.WithSourceRange(includeSource)))
	b, err := exp.Marshal(doc, []byte(source))
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	return b
}

func TestExtensionSnapshot(t *testing.T) {
	source := "| left | center | right |\n|:-----|:------:|------:|\n| ~~a~~|  `c`   |  1    |\n\n- [x] done\n- [ ] todo\n\nterm\n: def\n\nSee [^one].\n\n[^one]: note\n"

	withSource := exportExtensions(t, source, true)
	withoutSource := exportExtensions(t, source, false)

	if !json.Valid(withSource) || !json.Valid(withoutSource) {
		t.Fatal("extension output must be valid JSON")
	}

	goldenCheckExtension(t, "snapshot_extensions.golden.json", withSource)
	goldenCheckExtension(t, "snapshot_extensions_nosource.golden.json", withoutSource)

	// Structural sanity: table alignment and task status attribute survive.
	if !bytes.Contains(withSource, []byte(`"type": "Table"`)) ||
		!bytes.Contains(withSource, []byte(`"alignment": "center"`)) {
		t.Fatalf("table structure missing:\n%s", withSource)
	}
	if !bytes.Contains(withoutSource, []byte(`"task-status"`)) ||
		!bytes.Contains(withoutSource, []byte(`"completed"`)) {
		t.Fatalf("task list attribute missing:\n%s", withoutSource)
	}
	if !bytes.Contains(withoutSource, []byte(`"type": "Strikethrough"`)) ||
		!bytes.Contains(withoutSource, []byte(`"type": "FootnoteReference"`)) ||
		!bytes.Contains(withoutSource, []byte(`"type": "DefinitionList"`)) {
		t.Fatalf("extension node types missing:\n%s", withoutSource)
	}
}

func TestExtensionSnapshotIsByteStable(t *testing.T) {
	source := "| a | b |\n|---|---|\n| c | d |\n\n~~x~~\n"
	doc := extensionParser().Parse([]byte(source))
	exp := extension.RegisterAdaptersOn(astjson.NewExporter(astjson.WithSourceRange(true)))

	first, err := exp.Marshal(doc, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := exp.Marshal(doc, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("extension export not byte-stable on run %d", i)
		}
	}
}

func goldenCheckExtension(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create)", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}
