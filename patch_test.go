package validator

import (
	"testing"
	"time"
)

type patchSchedule struct {
	Start time.Time `validate:"required"`
	End   time.Time `validate:"required,gtfield=Start"`
}

type patchPointerSchedule struct {
	Start *time.Time `validate:"required"`
	End   *time.Time `validate:"required,gtfield=Start"`
}

type patchWindow struct {
	Start time.Time `validate:"required"`
	End   time.Time `validate:"required,gtfield=Start"`
}

type patchWindowRequest struct {
	Window *patchWindow
}

type patchProfile struct {
	Name *string
}

type patchRequiredProfile struct {
	Name *string `validate:"required"`
}

type patchRequest struct {
	Profile *patchProfile
}

type patchRequiredRequest struct {
	Profile *patchRequiredProfile
}

func TestStructPatchCrossFieldUsesOldDependency(t *testing.T) {
	validate := New()
	old := patchSchedule{
		Start: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	}

	patch := old
	patch.End = old.Start.Add(-time.Hour)

	err := validate.StructPatch(old, patch, "End")
	if err == nil {
		t.Fatal("expected cross-field validation error")
	}

	errs := err.(ValidationErrors)
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %d", len(errs))
	}

	if errs[0].Namespace() != "patchSchedule.End" || errs[0].Tag() != "gtfield" {
		t.Fatalf("unexpected error: namespace=%s tag=%s", errs[0].Namespace(), errs[0].Tag())
	}
}

func TestStructPatchExplicitNullDependencyDoesNotUseOldValue(t *testing.T) {
	validate := New()
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	old := patchPointerSchedule{Start: &start, End: &end}
	patch := patchPointerSchedule{Start: nil, End: &end}

	err := validate.StructPatch(old, patch, "Start", "End")
	if err == nil {
		t.Fatal("expected explicit null dependency to fail")
	}

	errs := err.(ValidationErrors)
	if len(errs) != 2 ||
		errs[0].Namespace() != "patchPointerSchedule.Start" || errs[0].Tag() != "required" ||
		errs[1].Namespace() != "patchPointerSchedule.End" || errs[1].Tag() != "gtfield" {
		t.Fatalf("unexpected errors: %+v", errs)
	}
}

func TestStructPatchNestedCrossFieldUsesOldDependency(t *testing.T) {
	validate := New()
	old := patchWindowRequest{
		Window: &patchWindow{
			Start: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
			End:   time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
		},
	}
	patch := patchWindowRequest{
		Window: &patchWindow{
			End: old.Window.Start.Add(-time.Hour),
		},
	}

	err := validate.StructPatch(old, patch, "Window.End")
	if err == nil {
		t.Fatal("expected nested cross-field validation error")
	}

	errs := err.(ValidationErrors)
	if len(errs) != 1 || errs[0].Namespace() != "patchWindowRequest.Window.End" || errs[0].Tag() != "gtfield" {
		t.Fatalf("unexpected errors: %+v", errs)
	}
}

func TestStructPatchPointerNullIsPresent(t *testing.T) {
	validate := New()
	old := patchRequiredRequest{
		Profile: &patchRequiredProfile{Name: stringPtr("Ada")},
	}

	patch := patchRequiredRequest{Profile: &patchRequiredProfile{Name: nil}}

	err := validate.StructPatch(old, patch, "Profile.Name")
	if err == nil {
		t.Fatal("expected explicit null to fail required validation")
	}

	errs := err.(ValidationErrors)
	if len(errs) != 1 || errs[0].Namespace() != "patchRequiredRequest.Profile.Name" || errs[0].Tag() != "required" {
		t.Fatalf("unexpected errors: %+v", errs)
	}

	patch.Profile.Name = stringPtr("Grace")
	if err := validate.StructPatch(old, patch, "Profile"); err != nil {
		t.Fatalf("expected nested object patch to pass, got %v", err)
	}
}

func TestStructPatchNestedCallbackSeesPresenceAndOldValue(t *testing.T) {
	validate := New()
	validate.RegisterStructValidationPatch(func(pl PatchLevel) {
		oldProfile := pl.Old().Interface().(patchProfile)
		current := pl.Current().Interface().(patchProfile)

		if !pl.Present("Name") {
			return
		}

		if current.Name == nil && oldProfile.Name != nil {
			pl.ReportError(current.Name, "Name", "Name", "required", "")
		}
	}, patchProfile{})

	old := patchRequest{
		Profile: &patchProfile{Name: stringPtr("Ada")},
	}

	t.Run("nested presence and old value", func(t *testing.T) {
		patch := patchRequest{Profile: &patchProfile{Name: nil}}

		err := validate.StructPatch(old, patch, "Profile.Name")
		if err == nil {
			t.Fatal("expected struct-level error for explicit null")
		}

		errs := err.(ValidationErrors)
		if len(errs) != 1 || errs[0].Namespace() != "patchRequest.Profile.Name" {
			t.Fatalf("unexpected errors: %+v", errs)
		}
	})

	t.Run("omitted zero value is not treated as present", func(t *testing.T) {
		patch := patchRequest{Profile: &patchProfile{}}

		if err := validate.StructPatch(old, patch, "Profile"); err != nil {
			t.Fatalf("expected omitted nested field to be skipped, got %v", err)
		}
	})
}
