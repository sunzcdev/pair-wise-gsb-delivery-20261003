package jsonquery

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, s string) *Node {
	t.Helper()
	doc, err := Parse(strings.NewReader(s))
	require.NoError(t, err)
	return doc
}

func employeePlan(t *testing.T) *ProjectionPlan {
	t.Helper()
	plan, err := CompileProjection("/company/teams/*/members/*", []Column{
		{Name: "id", XPath: "id", Required: true, Type: TypeNumber},
		{Name: "name", XPath: "name", Required: true, Type: TypeString},
		{Name: "score", XPath: "score", Type: TypeNumber, Default: 0},
		{Name: "nick", XPath: "nick", Type: TypeString, Default: "anon"},
		{Name: "active", XPath: "active", Type: TypeBool},
		{Name: "tags", XPath: "tags/*", Multi: true, Type: TypeString},
		{Name: "priceText", XPath: "price", Type: TypeString, Convert: AsText},
	})
	require.NoError(t, err)
	return plan
}

const projectionDoc = `{
  "company": {
    "teams": [
      {
        "members": [
          {
            "id": 1,
            "name": "Ada",
            "score": 9.5,
            "nick": null,
            "active": true,
            "tags": ["go", "xpath"],
            "price": 12
          },
          {
            "id": 2,
            "name": "Grace",
            "active": false,
            "tags": ["compiler"],
            "price": 8.25
          },
          {
            "id": 3,
            "name": "Linus",
            "score": 10,
            "nick": "penguin",
            "active": true,
            "tags": [],
            "price": 19.99
          }
        ]
      }
    ]
  }
}`

func TestProjectRowByRow(t *testing.T) {
	plan := employeePlan(t)
	rows := plan.Project(mustParse(t, projectionDoc))
	require.Len(t, rows, 3)

	// Row 1: fully successful; explicit null must win over the default.
	ada := rows[0]
	require.False(t, ada.Failed())
	require.Len(t, ada.Cells, 7)
	require.Equal(t, []string{"id", "name", "score", "nick", "active", "tags", "priceText"},
		cellNames(ada.Cells))
	require.Equal(t, float64(1), ada.Cells[0].Value)
	require.Equal(t, "Ada", ada.Cells[1].Value)
	require.Equal(t, float64(9.5), ada.Cells[2].Value)

	nick := ada.Cells[3]
	require.True(t, nick.Set, "explicit null must be marked set")
	require.Nil(t, nick.Value)

	require.Equal(t, true, ada.Cells[4].Value)
	require.Equal(t, []interface{}{"go", "xpath"}, ada.Cells[5].Value)
	require.Equal(t, "12", ada.Cells[6].Value)

	// Row 2: score missing -> default; tags has one element.
	grace := rows[1]
	require.False(t, grace.Failed())
	require.Equal(t, float64(0), grace.Cells[2].Value)
	require.True(t, grace.Cells[2].Set, "default value is set")
	require.Equal(t, "anon", grace.Cells[3].Value)
	require.Equal(t, []interface{}{"compiler"}, grace.Cells[5].Value)
	require.Equal(t, "8.25", grace.Cells[6].Value)

	// Row 3: empty tags array matches zero nodes; optional multi column with no
	// default is simply unset, not a failure.
	linus := rows[2]
	require.False(t, linus.Failed())
	require.Equal(t, "penguin", linus.Cells[3].Value)
	tags := linus.Cells[5]
	require.False(t, tags.Set)
	require.Nil(t, tags.Value)

	values := ada.Values()
	require.Equal(t, float64(1), values["id"])
	require.Nil(t, values["nick"])
}

func TestProjectOneRowFailsOthersSucceed(t *testing.T) {
	doc := `{
	  "rows": [
	    {"id": 10, "name": "ok", "email": "a@example.com", "code": "A"},
	    {"id": "eleven", "name": "bad-type", "emails": ["x", "y"]},
	    {"id": 12, "name": "missing-required", "email": "c@example.com", "code": "C"}
	  ]
	}`
	plan, err := CompileProjection("/rows/*", []Column{
		{Name: "id", XPath: "id", Required: true, Type: TypeNumber},
		{Name: "name", XPath: "name", Required: true, Type: TypeString},
		{Name: "email", XPath: "email", Type: TypeString},
		{Name: "emails", XPath: "emails/*", Type: TypeString},
		{Name: "code", XPath: "code", Required: true, Type: TypeString},
	})
	require.NoError(t, err)

	rows := plan.Project(mustParse(t, doc))
	require.Len(t, rows, 3)

	require.False(t, rows[0].Failed())
	require.Equal(t, float64(10), rows[0].Cells[0].Value)

	bad := rows[1]
	require.True(t, bad.Failed())
	require.Len(t, bad.Diagnostics, 3)
	require.Equal(t, DiagType, bad.Diagnostics[0].Code)
	require.Equal(t, "id", bad.Diagnostics[0].Column)
	require.Equal(t, DiagMultiple, bad.Diagnostics[1].Code)
	require.Equal(t, "emails", bad.Diagnostics[1].Column)
	require.Equal(t, DiagMissing, bad.Diagnostics[2].Code)
	require.Equal(t, "code", bad.Diagnostics[2].Column)
	// Successful cells of the failed row are still returned.
	require.Equal(t, "bad-type", bad.Cells[1].Value)
	require.False(t, bad.Cells[0].Set)
	require.Nil(t, bad.Cells[2].Value)
	require.False(t, bad.Cells[2].Set)

	// The remaining row is evaluated independently and succeeds.
	last := rows[2]
	require.False(t, last.Failed())
	require.Equal(t, float64(12), last.Cells[0].Value)
	require.Equal(t, "missing-required", last.Cells[1].Value)
}

func TestProjectRequiredMissingAndMulti(t *testing.T) {
	plan, err := CompileProjection("/orders/*", []Column{
		{Name: "sku", XPath: "sku", Required: true, Type: TypeString},
		{Name: "notes", XPath: "notes/*", Required: true, Multi: true, Type: TypeString},
		{Name: "items", XPath: "items/*", Multi: true, Type: TypeObject},
	})
	require.NoError(t, err)

	doc := mustParse(t, `{
	  "orders": [
	    {"sku": "A1", "notes": ["n1"], "items": [{"id": 1}]},
	    {"notes": ["n2", "n3"]},
	    {"sku": "A3", "items": [{"id": 3}, {"id": 4}]}
	  ]
	}`)
	rows := plan.Project(doc)
	require.Len(t, rows, 3)

	require.False(t, rows[0].Failed())
	require.Equal(t, "A1", rows[0].Cells[0].Value)
	require.Equal(t, []interface{}{"n1"}, rows[0].Cells[1].Value)

	// Row 2 misses the required sku column.
	missingSku := rows[1]
	require.True(t, missingSku.Failed())
	require.Equal(t, DiagMissing, missingSku.Diagnostics[0].Code)
	require.Equal(t, "sku", missingSku.Diagnostics[0].Column)
	require.Equal(t, []interface{}{"n2", "n3"}, missingSku.Cells[1].Value)

	// Row 3 misses the required multi-value notes column.
	missingNotes := rows[2]
	require.True(t, missingNotes.Failed())
	require.Len(t, missingNotes.Diagnostics, 1)
	require.Equal(t, DiagMissing, missingNotes.Diagnostics[0].Code)
	require.Equal(t, "notes", missingNotes.Diagnostics[0].Column)
	require.Equal(t, []interface{}{
		map[string]interface{}{"id": float64(3)},
		map[string]interface{}{"id": float64(4)},
	}, missingNotes.Cells[2].Value)
}

func TestProjectOptionalMultiMissing(t *testing.T) {
	plan, err := CompileProjection("/orders/*", []Column{
		{Name: "sku", XPath: "sku", Type: TypeString},
		{Name: "notes", XPath: "notes/*", Multi: true, Type: TypeString},
	})
	require.NoError(t, err)

	rows := plan.Project(mustParse(t, `{"orders":[{"sku": "A3"}]}`))
	require.Len(t, rows, 1)
	row := rows[0]
	require.False(t, row.Failed())
	require.Equal(t, "A3", row.Cells[0].Value)
	notes := row.Cells[1]
	require.False(t, notes.Set)
	require.Nil(t, notes.Value)
	require.Empty(t, notes.Diagnostics)
}

func TestProjectExplicitConvertRule(t *testing.T) {
	plan, err := CompileProjection("/events/*", []Column{
		{Name: "code", XPath: "code", Required: true, Type: TypeNumber},
		{Name: "label", XPath: "label", Required: true, Type: TypeString, Convert: AsText},
	})
	require.NoError(t, err)

	rows := plan.Project(mustParse(t, `{
	  "events": [
	    {"code": 1, "label": 200},
	    {"code": 2, "label": true},
	    {"code": "three", "label": "ok"},
	    {"code": 4, "label": {"nested": true}}
	  ]
	}`))
	require.Len(t, rows, 4)

	require.Equal(t, "200", rows[0].Cells[1].Value)
	require.Equal(t, "true", rows[1].Cells[1].Value)

	require.True(t, rows[2].Failed())
	require.Equal(t, DiagType, rows[2].Diagnostics[0].Code)
	require.Equal(t, "code", rows[2].Diagnostics[0].Column)

	require.True(t, rows[3].Failed())
	var convertDiag *Diagnostic
	for i := range rows[3].Diagnostics {
		if rows[3].Diagnostics[i].Code == DiagConvert {
			convertDiag = &rows[3].Diagnostics[i]
		}
	}
	require.NotNil(t, convertDiag)
	require.Equal(t, "label", convertDiag.Column)

	// Without an explicit rule, no implicit conversion is performed.
	strict, err := CompileProjection("/events/*", []Column{
		{Name: "label", XPath: "label", Type: TypeString},
	})
	require.NoError(t, err)
	strictRows := strict.Project(mustParse(t, `{"events":[{"label": 200}]}`))
	require.True(t, strictRows[0].Failed())
	require.Equal(t, DiagType, strictRows[0].Diagnostics[0].Code)
}

func TestProjectPlanReusedAcrossDocuments(t *testing.T) {
	plan := employeePlan(t)

	first := mustParse(t, projectionDoc)
	firstRows := plan.Project(first)
	require.Len(t, firstRows, 3)
	firstRowNode := firstRows[0].Node

	// Apply the same plan to a second document. Row pointers must belong to the
	// new document and none may alias nodes of the first document.
	second := mustParse(t, `{
	  "company": {"teams": [{"members": [
	    {"id": 99, "name": "Second", "active": true, "tags": [], "price": 1}
	  ]}]}
	}`)
	secondRows := plan.Project(second)
	require.Len(t, secondRows, 1)
	require.Equal(t, "Second", secondRows[0].Cells[1].Value)
	require.Equal(t, float64(99), secondRows[0].Cells[0].Value)

	secondRowNode := secondRows[0].Node
	require.True(t, nodeInDocument(secondRowNode, second))
	require.False(t, nodeInDocument(secondRowNode, first))
	require.False(t, nodeInDocument(firstRowNode, second))

	// Re-running against the first document must still resolve its own nodes.
	again := plan.Project(first)
	require.Len(t, again, 3)
	require.Equal(t, "Ada", again[0].Cells[1].Value)
	require.True(t, nodeInDocument(again[0].Node, first))
}

func TestCompileProjectionValidation(t *testing.T) {
	cases := []struct {
		name    string
		rowPath string
		columns []Column
	}{
		{"bad row xpath", ":::", []Column{{Name: "a", XPath: "a"}}},
		{"no columns", "/rows/*", nil},
		{"empty column name", "/rows/*", []Column{{XPath: "a"}}},
		{"duplicate name", "/rows/*", []Column{
			{Name: "a", XPath: "a"},
			{Name: "a", XPath: "b"},
		}},
		{"empty column xpath", "/rows/*", []Column{{Name: "a", XPath: ""}}},
		{"bad column xpath", "/rows/*", []Column{{Name: "a", XPath: "[["}}},
		{"default on required", "/rows/*", []Column{
			{Name: "a", XPath: "a", Required: true, Default: "x"},
		}},
		{"default wrong type", "/rows/*", []Column{
			{Name: "a", XPath: "a", Type: TypeNumber, Default: "x"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileProjection(tc.rowPath, tc.columns)
			require.Error(t, err)
		})
	}

	plan, err := CompileProjection("/rows/*", []Column{
		{Name: "n", XPath: "n", Type: TypeNumber, Default: 7},
	})
	require.NoError(t, err)
	require.Equal(t, float64(7), plan.columns[0].column.Default)
}

func cellNames(cells []Cell) []string {
	names := make([]string, len(cells))
	for i, cell := range cells {
		names[i] = cell.Name
	}
	return names
}

func nodeInDocument(node, doc *Node) bool {
	if node == nil {
		return false
	}
	for cur := node; cur != nil; cur = cur.Parent {
		if cur == doc {
			return true
		}
	}
	return false
}
