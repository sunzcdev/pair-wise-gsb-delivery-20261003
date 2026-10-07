package jsonquery

import (
	"reflect"
	"strings"
	"testing"
)

const projectionFixture = `{
	"items": [
		{"id": 1, "name": "alpha", "tags": ["a", "b"], "note": null},
		{"id": 2, "tags": ["c"], "note": "keep me"},
		{"id": 3, "name": "gamma", "tags": [], "note": "x"}
	]
}`

func TestProjectRowsIndependentResults(t *testing.T) {
	doc, err := parseString(projectionFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan := MustPlan("//items/*",
		Column{Name: "id", XPath: "id", Required: true, Type: NumberType},
		Column{Name: "name", XPath: "name", Required: true, Type: StringType},
		Column{Name: "tags", XPath: "tags/*", Multi: true, Type: StringType},
	)
	res := plan.Project(doc)
	if len(res.Rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(res.Rows))
	}
	if !res.Rows[0].OK() {
		t.Fatalf("row 0 should be ok, got %v", res.Rows[0].Diagnostics)
	}
	if res.Rows[1].OK() {
		t.Fatal("row 1 should have a diagnostic for missing required column name")
	}
	if got := res.Rows[1].Diagnostics[0].Column; got != "name" {
		t.Fatalf("row 1 diagnostic should be on column name, got %q", got)
	}
	if !res.Rows[2].OK() {
		t.Fatalf("row 2 should be ok, got %v", res.Rows[2].Diagnostics)
	}
	// Failing row still carries the values that could be projected.
	if got := res.Rows[1].Values[0]; got != float64(2) {
		t.Fatalf("row 1 id = %v, want 2", got)
	}
	if got := res.Rows[0].Values[2]; !reflect.DeepEqual(got, []interface{}{"a", "b"}) {
		t.Fatalf("row 0 tags = %v", got)
	}
}

func TestProjectColumnOrderAndDefaultVsNull(t *testing.T) {
	doc, err := parseString(projectionFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan := MustPlan("//items/*",
		Column{Name: "note", XPath: "note", Type: StringType, Default: "(none)", HasDefault: true},
		Column{Name: "missing", XPath: "missing", Default: "fallback", HasDefault: true},
		Column{Name: "id", XPath: "id", Type: NumberType},
	)
	res := plan.Project(doc)
	for i, row := range res.Rows {
		if !row.OK() {
			t.Fatalf("row %d should be ok, got %v", i, row.Diagnostics)
		}
	}
	// Values follow the declared column order: note, missing, id.
	if got := res.Rows[0].Values[0]; got != nil {
		t.Fatalf("explicit null must not be replaced by default, got %v", got)
	}
	if got := res.Rows[0].Values[1]; got != "fallback" {
		t.Fatalf("missing column should use default, got %v", got)
	}
	if got := res.Rows[0].Values[2]; got != float64(1) {
		t.Fatalf("third value should be id, got %v", got)
	}
	if got := res.Rows[1].Values[0]; got != "keep me" {
		t.Fatalf("row 1 note = %v", got)
	}
}

func TestProjectSingleValueColumnMultipleMatches(t *testing.T) {
	doc, err := parseString(projectionFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan := MustPlan("//items/*",
		Column{Name: "tag", XPath: "tags/*", Type: StringType},
	)
	res := plan.Project(doc)
	if res.Rows[0].OK() {
		t.Fatal("row 0 should report a diagnostic: two tags matched a single-value column")
	}
	if !strings.Contains(res.Rows[0].Diagnostics[0].Message, "2 nodes") {
		t.Fatalf("unexpected diagnostic: %v", res.Rows[0].Diagnostics[0])
	}
	if !res.Rows[1].OK() || res.Rows[1].Values[0] != "c" {
		t.Fatalf("row 1 should project the single tag, got %v %v", res.Rows[1].Values, res.Rows[1].Diagnostics)
	}
}

func TestProjectConversionRequiresExplicitRule(t *testing.T) {
	doc, err := parseString(`{"items":[{"id":1,"name":"alpha"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	strict := MustPlan("//items/*",
		Column{Name: "id", XPath: "id", Type: StringType},
	)
	row := strict.Project(doc).Rows[0]
	if row.OK() {
		t.Fatal("number-to-string without an explicit rule should be a diagnostic")
	}

	converted := MustPlan("//items/*",
		Column{Name: "id", XPath: "id", Type: StringType, Rules: NumberToString},
	)
	row = converted.Project(doc).Rows[0]
	if !row.OK() || row.Values[0] != "1" {
		t.Fatalf("explicit NumberToString rule should convert, got %v %v", row.Values, row.Diagnostics)
	}
}

func TestPlanReusableAcrossDocuments(t *testing.T) {
	plan := MustPlan("//items/*",
		Column{Name: "id", XPath: "id", Required: true, Type: NumberType},
	)
	doc1, err := parseString(`{"items":[{"id":1},{"id":2}]}`)
	if err != nil {
		t.Fatal(err)
	}
	doc2, err := parseString(`{"items":[{"id":7}]}`)
	if err != nil {
		t.Fatal(err)
	}
	res1 := plan.Project(doc1)
	res2 := plan.Project(doc2)
	if len(res1.Rows) != 2 || len(res2.Rows) != 1 {
		t.Fatalf("unexpected row counts: %d, %d", len(res1.Rows), len(res2.Rows))
	}
	if got := res2.Rows[0].Values[0]; got != float64(7) {
		t.Fatalf("second document projected stale value %v", got)
	}
	if got := res1.Rows[0].Values[0]; got != float64(1) {
		t.Fatalf("first document result changed after reuse: %v", got)
	}
}

func TestNewPlanInvalidXPath(t *testing.T) {
	if _, err := NewPlan("//items/*[", Column{Name: "id", XPath: "id"}); err == nil {
		t.Fatal("expected error for invalid row XPath")
	}
	if _, err := NewPlan("//items/*", Column{Name: "bad", XPath: "///"}); err == nil {
		t.Fatal("expected error for invalid column XPath")
	}
}
