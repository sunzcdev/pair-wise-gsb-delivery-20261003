package validator

import (
	"context"
	"reflect"
	"testing"
	"time"

	. "github.com/go-playground/assert/v2"
)

// ---- types under test -----------------------------------------------------

type patchWindow struct {
	Start *time.Time `json:"start" validate:"required"`
	End   *time.Time `json:"end" validate:"required,gtfield=Start"`
}

type patchBooking struct {
	Window *patchWindow `json:"window" validate:"required"`
	Note   *string      `json:"note" validate:"required"`
}

type patchDoc struct {
	Title  string   `json:"title" validate:"required"`
	Labels []string `json:"labels" validate:"required,dive,min=2"`
}

type patchItem struct {
	Name string `json:"name" validate:"required"`
}

type patchOrder struct {
	Items []patchItem `json:"items" validate:"dive"`
}

type patchInner struct {
	Value string `json:"value" validate:"required_if=Mode x"`
	Mode  string `json:"mode"`
}

type patchOuter struct {
	Inner patchInner `json:"inner"`
}

func patchTime(hour int) *time.Time {
	t := time.Date(2026, 10, 1, hour, 0, 0, 0, time.UTC)
	return &t
}

func patchStr(s string) *string { return &s }

// ---- PatchFieldsFromJSON --------------------------------------------------

func TestPatchFieldsFromJSON(t *testing.T) {
	// null and zero values are recorded as present
	pf, err := PatchFieldsFromJSON(patchBooking{}, []byte(`{"note":null,"window":{"end":null}}`))
	Equal(t, err, nil)
	Equal(t, pf.Has("Note"), true)
	Equal(t, pf.Has("Window"), true)
	Equal(t, pf.Has("Window.End"), true)
	Equal(t, pf.Has("Window.Start"), false)

	// omitted fields are absent
	pf, err = PatchFieldsFromJSON(patchDoc{}, []byte(`{}`))
	Equal(t, err, nil)
	Equal(t, len(pf), 0)

	// zero value string still present
	pf, err = PatchFieldsFromJSON(patchDoc{}, []byte(`{"title":""}`))
	Equal(t, err, nil)
	Equal(t, pf.Has("Title"), true)

	// json tags + case insensitive Go field name + nested arrays/maps
	type entry struct {
		Name string `json:"name"`
	}
	type withColl struct {
		Items []entry          `json:"items"`
		Map   map[string]entry `json:"map"`
	}
	body := []byte(`{"items":[{"name":"a"}],"map":{"k":{"name":"b"}}}`)
	pf, err = PatchFieldsFromJSON(withColl{}, body)
	Equal(t, err, nil)
	Equal(t, pf.Has("Items"), true)
	Equal(t, pf.Has("Items[0]"), true)
	Equal(t, pf.Has("Items[0].Name"), true)
	Equal(t, pf.Has("Map[k].Name"), true)

	// json null body -> empty set
	pf, err = PatchFieldsFromJSON(patchDoc{}, []byte(`null`))
	Equal(t, err, nil)
	Equal(t, len(pf), 0)

	// invalid json surfaces the decode error
	_, err = PatchFieldsFromJSON(patchDoc{}, []byte(`{not json`))
	NotEqual(t, err, nil)

	// non-struct is an InvalidValidationError
	_, err = PatchFieldsFromJSON(5, []byte(`{}`))
	_, ok := err.(*InvalidValidationError)
	Equal(t, ok, true)

	// NewPatchFields + Add + Has
	pf = NewPatchFields("Window.End").Add("Note")
	Equal(t, pf.Has("Window.End"), true)
	Equal(t, pf.Has("Note"), true)
	Equal(t, pf.Has("Window"), false)
}

// ---- StructPatch: cross-field uses old state ------------------------------

func TestStructPatchCrossFieldUsesOldState(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	old := &patchBooking{Window: &patchWindow{Start: patchTime(10)}}

	// Only End submitted; it is compared against the OLD Start and passes.
	p := &patchBooking{Window: &patchWindow{End: patchTime(12)}}
	pf := NewPatchFields("Window.End")
	Equal(t, validate.StructPatch(p, old, pf), nil)

	// End earlier than the OLD Start fails, with the error on submitted End.
	p = &patchBooking{Window: &patchWindow{End: patchTime(8)}}
	err := validate.StructPatch(p, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchBooking.Window.End", "patchBooking.Window.End", "End", "End", "gtfield")
}

// ---- StructPatch: explicit null vs omitted --------------------------------

func TestStructPatchExplicitNullVsOmitted(t *testing.T) {
	validate := New(WithRequiredStructEnabled())
	old := &patchBooking{Window: &patchWindow{Start: patchTime(10)}}

	// explicit null for note -> present + nil -> required fails on Note
	pf, err := PatchFieldsFromJSON(patchBooking{}, []byte(`{"note":null}`))
	Equal(t, err, nil)
	err = validate.StructPatch(&patchBooking{}, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchBooking.Note", "patchBooking.Note", "Note", "Note", "required")

	// note omitted entirely -> not validated -> passes
	pf, err = PatchFieldsFromJSON(patchBooking{}, []byte(`{"window":{"end":null}}`))
	Equal(t, err, nil)
	err = validate.StructPatch(&patchBooking{Window: &patchWindow{}}, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchBooking.Window.End", "patchBooking.Window.End", "End", "End", "required")

	// zero value submitted for a non-pointer field still counts as present
	pfStr, err := PatchFieldsFromJSON(patchDoc{}, []byte(`{"title":""}`))
	Equal(t, err, nil)
	err = validate.StructPatch(&patchDoc{}, nil, pfStr)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchDoc.Title", "patchDoc.Title", "Title", "Title", "required")

	// nested pointer explicitly null -> error on the submitted pointer field
	pfNull, err := PatchFieldsFromJSON(patchBooking{}, []byte(`{"window":null}`))
	Equal(t, err, nil)
	err = validate.StructPatch(&patchBooking{}, old, pfNull)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchBooking.Window", "patchBooking.Window", "Window", "Window", "required")
}

// ---- StructPatch: nested object, only one subfield ------------------------

func TestStructPatchNestedObject(t *testing.T) {
	validate := New(WithRequiredStructEnabled())
	old := &patchBooking{Window: &patchWindow{Start: patchTime(10), End: patchTime(12)}}

	// replace Start only; End omitted: Start passes required, End skipped.
	pf := NewPatchFields("Window.Start")
	p := &patchBooking{Window: &patchWindow{Start: patchTime(20)}}
	Equal(t, validate.StructPatch(p, old, pf), nil)

	// body-derived mask matches the Go-field mask
	pfJSON, err := PatchFieldsFromJSON(patchBooking{}, []byte(`{"window":{"start":"2026-10-01T20:00:00Z"}}`))
	Equal(t, err, nil)
	Equal(t, validate.StructPatch(p, old, pfJSON), nil)
}

// ---- StructPatch: nested slices -------------------------------------------

func TestStructPatchNestedSlice(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	old := &patchOrder{Items: []patchItem{{Name: "old"}}}

	// item name present but empty -> required fails at the submitted index
	pf := NewPatchFields("Items[0].Name")
	p := &patchOrder{Items: []patchItem{{Name: ""}}}
	err := validate.StructPatch(p, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchOrder.Items[0].Name", "patchOrder.Items[0].Name", "Name", "Name", "required")

	// valid item passes
	p = &patchOrder{Items: []patchItem{{Name: "new"}}}
	Equal(t, validate.StructPatch(p, old, pf), nil)

	// JSON-derived flattened index path
	pfJSON, err := PatchFieldsFromJSON(patchOrder{}, []byte(`{"items":[{"name":""}]}`))
	Equal(t, err, nil)
	p = &patchOrder{Items: []patchItem{{Name: ""}}}
	err = validate.StructPatch(p, old, pfJSON)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchOrder.Items[0].Name", "patchOrder.Items[0].Name", "Name", "Name", "required")
}

// ---- StructPatch: conditional tags fall back to old state -----------------

func TestStructPatchRequiredIfFallsBackToOld(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	// Mode=x lives only in the old state; Value omitted in the patch.
	old := &patchOuter{Inner: patchInner{Mode: "x"}}

	pf := NewPatchFields("Inner.Value")
	p := &patchOuter{Inner: patchInner{Value: ""}}
	err := validate.StructPatch(p, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchOuter.Inner.Value", "patchOuter.Inner.Value", "Value", "Value", "required_if")

	// old Mode is not x -> rule does not fire -> passes
	oldOK := &patchOuter{Inner: patchInner{Mode: "y"}}
	Equal(t, validate.StructPatch(p, oldOK, pf), nil)
}

// ---- StructPatch: PatchFieldLevel / PatchLevel access ---------------------

func TestStructPatchCustomFuncsSeePresenceAndOld(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	type change struct {
		Name string `validate:"patch_changed"`
	}

	var sawPresent bool
	var sawOld reflect.Value

	err := validate.RegisterValidation("patch_changed", func(fl FieldLevel) bool {
		pl, ok := fl.(PatchFieldLevel)
		if !ok {
			return true
		}
		sawPresent = pl.IsPresent("Name")
		sawOld = pl.OldField()
		return true
	})
	Equal(t, err, nil)

	var structSaw bool
	validate.RegisterStructValidation(func(sl StructLevel) {
		if pl, ok := sl.(PatchLevel); ok {
			structSaw = pl.IsPresent("Name")
			_ = pl.OldTop()
		}
	}, change{})

	old := &change{Name: "before"}
	p := &change{Name: "after"}
	Equal(t, validate.StructPatch(p, old, NewPatchFields("Name")), nil)
	Equal(t, sawPresent, true)
	Equal(t, sawOld.IsValid(), true)
	Equal(t, sawOld.String(), "before")
	Equal(t, structSaw, true)

	// same context outside patch mode must not implement the patch interfaces
	err = validate.Struct(&change{Name: "x"})
	Equal(t, err, nil)
}

// ---- StructPatch: entry point validation ----------------------------------

type patchCSInner struct {
	Start *time.Time `json:"start"`
}

type patchCSOuter struct {
	Inner *patchCSInner `json:"inner"`
	End   *time.Time    `json:"end" validate:"gtcsfield=Inner.Start"`
}

func TestStructPatchCrossStructOldFallback(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	old := &patchCSOuter{Inner: &patchCSInner{Start: patchTime(10)}}

	// only End submitted; gtcsfield resolves Inner.Start against the old top
	p := &patchCSOuter{End: patchTime(12)}
	Equal(t, validate.StructPatch(p, old, NewPatchFields("End")), nil)

	p = &patchCSOuter{End: patchTime(8)}
	err := validate.StructPatch(p, old, NewPatchFields("End"))
	NotEqual(t, err, nil)
	AssertError(t, err, "patchCSOuter.End", "patchCSOuter.End", "End", "End", "gtcsfield")
}

type patchMapDoc struct {
	Entries map[string]patchItem `json:"entries"`
}

func TestStructPatchMapValues(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	old := &patchMapDoc{Entries: map[string]patchItem{"k": {Name: "old"}}}

	pf, err := PatchFieldsFromJSON(patchMapDoc{}, []byte(`{"entries":{"k":{"name":""}}}`))
	Equal(t, err, nil)

	p := &patchMapDoc{Entries: map[string]patchItem{"k": {Name: ""}}}
	err = validate.StructPatch(p, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchMapDoc.Entries[k].Name", "patchMapDoc.Entries[k].Name", "Name", "Name", "required")

	// untouched keys are not validated even though the decoded map carries them
	pfOnlyA, err := PatchFieldsFromJSON(patchMapDoc{}, []byte(`{"entries":{"a":{"name":"x"}}}`))
	Equal(t, err, nil)
	p2 := &patchMapDoc{Entries: map[string]patchItem{"a": {Name: "x"}, "b": {Name: ""}}}
	Equal(t, validate.StructPatch(p2, old, pfOnlyA), nil)
}

type patchEmbeddedBase struct {
	City string `json:"city" validate:"required"`
}

type patchEmbedded struct {
	patchEmbeddedBase
	Age int `json:"age" validate:"gte=0"`
}

func TestStructPatchAnonymousEmbedded(t *testing.T) {
	validate := New(WithRequiredStructEnabled())

	old := &patchEmbedded{patchEmbeddedBase: patchEmbeddedBase{City: "Berlin"}, Age: 5}

	// promoted json key "city"; error namespace includes the embedding segment
	pf, err := PatchFieldsFromJSON(patchEmbedded{}, []byte(`{"city":""}`))
	Equal(t, err, nil)
	err = validate.StructPatch(&patchEmbedded{}, old, pf)
	NotEqual(t, err, nil)
	AssertError(t, err, "patchEmbedded.patchEmbeddedBase.City", "patchEmbedded.patchEmbeddedBase.City", "City", "City", "required")

	// only age submitted; promoted city omitted -> not validated
	Equal(t, validate.StructPatch(&patchEmbedded{Age: 7}, old, NewPatchFields("Age")), nil)
}

func TestStructPatchInvalidInputs(t *testing.T) {
	validate := New()

	err := validate.StructPatch(5, nil, NewPatchFields())
	_, isInvalid := err.(*InvalidValidationError)
	Equal(t, isInvalid, true)

	// old of a different type is invalid
	err = validate.StructPatch(&patchDoc{}, &patchOrder{}, NewPatchFields())
	_, isInvalid = err.(*InvalidValidationError)
	Equal(t, isInvalid, true)

	// context variant behaves identically for a valid call
	Equal(t, validate.StructPatchCtx(context.Background(),
		&patchBooking{Window: &patchWindow{End: patchTime(12)}},
		&patchBooking{Window: &patchWindow{Start: patchTime(10)}},
		NewPatchFields("Window.End"),
	), nil)
}
