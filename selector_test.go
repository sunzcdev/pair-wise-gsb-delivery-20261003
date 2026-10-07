package vcard

import "testing"

func fieldWith(value string, params Params) *Field {
	return &Field{Value: value, Params: params}
}

func TestSelector_NoProperty(t *testing.T) {
	card := Card{}
	if got := card.Select(FieldAddress, WithLanguages("zh")); got != nil {
		t.Errorf("expected nil for absent property, got %+v", got)
	}
	if got := card.SelectAll(FieldAddress); got != nil {
		t.Errorf("expected nil slice for absent property, got %+v", got)
	}
}

func TestSelector_StructuredPropertyChosenWhole(t *testing.T) {
	zh := fieldWith("张;三;;;", Params{
		ParamLanguage: {"zh"},
		ParamAltID:    {"1"},
	})
	en := fieldWith("Zhang;San;;;", Params{
		ParamLanguage: {"en"},
		ParamAltID:    {"1"},
	})
	card := Card{FieldName: {zh, en}}

	got := card.Select(FieldName, WithLanguages("zh"))
	if got != zh {
		t.Fatalf("expected the whole Chinese N field, got %+v", got)
	}
	if got.Value != "张;三;;;" {
		t.Errorf("expected whole structured value %q, got %q", "张;三;;;", got.Value)
	}

	got = card.Select(FieldName, WithLanguages("en"))
	if got != en {
		t.Fatalf("expected the whole English N field, got %+v", got)
	}
}

func TestSelector_MissingPreferredFallsBackToUntagged(t *testing.T) {
	en := fieldWith("London", Params{ParamLanguage: {"en"}, ParamType: {"home"}})
	untagged := fieldWith("London (no lang)", Params{ParamType: {"work"}})
	card := Card{FieldAddress: {en, untagged}}

	// Default selector falls back to an untagged variant.
	got := card.Select(FieldAddress, WithLanguages("zh"))
	if got != untagged {
		t.Fatalf("expected untagged fallback, got %+v", got)
	}

	// Strict selector preserves the explicit "no match" state.
	if got := card.Select(FieldAddress, WithLanguages("zh"), WithUntaggedFallback(false)); got != nil {
		t.Fatalf("expected no match without untagged fallback, got %+v", got)
	}

	// Allowing any-language fallback surfaces the English variant.
	got = card.Select(FieldAddress, WithLanguages("zh"), WithUntaggedFallback(false), WithAnyLanguageFallback(true))
	if got != en {
		t.Fatalf("expected any-language fallback, got %+v", got)
	}
}

func TestSelector_MultipleTranslationsSamePrefTie(t *testing.T) {
	hans := fieldWith("简体", Params{
		ParamLanguage:  {"zh-Hans"},
		ParamAltID:     {"1"},
		ParamPreferred: {"1"},
	})
	hant := fieldWith("繁體", Params{
		ParamLanguage:  {"zh-Hant"},
		ParamAltID:     {"1"},
		ParamPreferred: {"1"},
	})
	card := Card{FieldFormattedName: {hans, hant}}

	tied := card.SelectAll(FieldFormattedName, WithLanguages("zh"))
	if len(tied) != 2 || tied[0] != hans || tied[1] != hant {
		t.Fatalf("expected both equally matched variants, got %+v", tied)
	}

	// Stable default rule keeps document order.
	if got := card.Select(FieldFormattedName, WithLanguages("zh")); got != hans {
		t.Fatalf("expected first variant by default tiebreaker, got %+v", got)
	}

	// Caller-supplied stable rule.
	got := card.Select(FieldFormattedName, WithLanguages("zh"), WithTiebreaker(func(candidates []*Field) *Field {
		return candidates[len(candidates)-1]
	}))
	if got != hant {
		t.Fatalf("expected caller-chosen variant, got %+v", got)
	}
}

func TestSelector_BasicLanguageFiltering(t *testing.T) {
	cn := fieldWith("zh-Hans-CN", Params{ParamLanguage: {"zh-Hans-CN"}, ParamAltID: {"1"}})
	hant := fieldWith("zh-Hant", Params{ParamLanguage: {"zh-Hant"}, ParamAltID: {"1"}})
	card := Card{FieldFormattedName: {cn, hant}}

	// A specific range only matches its subtree; language beats PREF.
	if got := card.Select(FieldFormattedName, WithLanguages("zh-Hans")); got != cn {
		t.Fatalf("expected zh-Hans-CN for range zh-Hans, got %+v", got)
	}
	// An exact tag outranks a broader partial match when both apply.
	hans := fieldWith("zh-Hans", Params{ParamLanguage: {"zh-Hans"}, ParamAltID: {"2"}, ParamPreferred: {"9"}})
	specific := fieldWith("zh-Hans-CN", Params{ParamLanguage: {"zh-Hans-CN"}, ParamAltID: {"2"}, ParamPreferred: {"1"}})
	exactCard := Card{FieldFormattedName: {hans, specific}}
	if got := exactCard.Select(FieldFormattedName, WithLanguages("zh-Hans")); got != hans {
		t.Fatalf("expected exact zh-Hans to outrank subtree match, got %+v", got)
	}

	// Under the broad "zh" range the Hans and Hant subtree matches are
	// genuinely different dialects and stay tied (PREF is language-scoped).
	if tied := card.SelectAll(FieldFormattedName, WithLanguages("zh-Hant")); len(tied) != 1 || tied[0] != hant {
		t.Fatalf("expected only zh-Hant for range zh-Hant, got %+v", tied)
	}
	if tied := card.SelectAll(FieldFormattedName, WithLanguages("zh")); len(tied) != 2 {
		t.Fatalf("expected both zh subtree matches tied for range zh, got %+v", tied)
	}
	// Case-insensitive matching.
	if got := card.Select(FieldFormattedName, WithLanguages("ZH-hans")); got != cn {
		t.Fatalf("expected case-insensitive match, got %+v", got)
	}
}

func TestSelector_LanguagePreferenceOrder(t *testing.T) {
	en := fieldWith("en", Params{ParamLanguage: {"en"}, ParamAltID: {"1"}, ParamPreferred: {"50"}})
	zh := fieldWith("zh", Params{ParamLanguage: {"zh"}, ParamAltID: {"1"}, ParamPreferred: {"1"}})
	card := Card{FieldEmail: {en, zh}}

	if got := card.Select(FieldEmail, WithLanguages("en", "zh")); got != en {
		t.Fatalf("ordered preference must beat PREF, got %+v", got)
	}
	if got := card.Select(FieldEmail, WithLanguages("zh", "en")); got != zh {
		t.Fatalf("expected zh as first preference, got %+v", got)
	}
}

func TestSelector_TypePurposeAndPref(t *testing.T) {
	zhWork := fieldWith("zh-work", Params{ParamLanguage: {"zh"}, ParamAltID: {"1"}, ParamType: {"work"}})
	zhHome := fieldWith("zh-home", Params{ParamLanguage: {"zh"}, ParamAltID: {"1"}, ParamType: {"home"}})
	enWork := fieldWith("en-work", Params{ParamLanguage: {"en"}, ParamAltID: {"1"}, ParamType: {"work"}})
	card := Card{FieldAddress: {zhWork, zhHome, enWork}}

	// Language first (zh), then TYPE purpose (work over home).
	got := card.Select(FieldAddress, WithLanguages("zh", "en"), WithTypes(TypeWork, TypeHome))
	if got != zhWork {
		t.Fatalf("expected zh work address, got %+v", got)
	}

	// English-only preference falls through to the English work address.
	got = card.Select(FieldAddress, WithLanguages("en"), WithTypes(TypeWork, TypeHome))
	if got != enWork {
		t.Fatalf("expected en work address, got %+v", got)
	}

	// Work vs home telephone across independent fields, broken by PREF.
	work := fieldWith("tel:work", Params{ParamType: {"work"}, ParamPreferred: {"2"}})
	home := fieldWith("tel:home", Params{ParamType: {"home"}, ParamPreferred: {"1"}})
	telCard := Card{FieldTelephone: {work, home}}
	if got := telCard.Select(FieldTelephone, WithTypes(TypeWork, TypeHome)); got != work {
		t.Fatalf("TYPE order must beat PREF, got %+v", got)
	}
	if got := telCard.Select(FieldTelephone, WithTypes(TypeHome, TypeWork)); got != home {
		t.Fatalf("expected home when preferred first, got %+v", got)
	}
}

func TestSelector_AltIDGroupsAreIndependent(t *testing.T) {
	zh := fieldWith("zh", Params{ParamLanguage: {"zh"}, ParamAltID: {"a"}, ParamPreferred: {"1"}})
	enP1 := fieldWith("en-pref1", Params{ParamLanguage: {"en"}, ParamAltID: {"b"}, ParamPreferred: {"1"}})
	enP2 := fieldWith("en-pref2", Params{ParamLanguage: {"en"}, ParamAltID: {"b"}, ParamPreferred: {"2"}})
	card := Card{FieldNote: {zh, enP1, enP2}}

	// Within group b the PREF=1 English variant wins; it then ties with the
	// zh variant because language preference is not configured.
	tied := card.SelectAll(FieldNote, WithTypes())
	if len(tied) != 2 || tied[0] != zh || tied[1] != enP1 {
		t.Fatalf("expected group winners zh and en-pref1, got %+v", tied)
	}

	// With an English preference only the English group's PREF=1 survives.
	got := card.Select(FieldNote, WithLanguages("en"))
	if got != enP1 {
		t.Fatalf("expected en PREF=1, got %+v", got)
	}
}

func TestSelector_UnknownParametersPreserved(t *testing.T) {
	zh := fieldWith("value", Params{
		ParamLanguage: {"zh"},
		ParamAltID:    {"1"},
		"X-CUSTOM":    {"kept"},
		"PID":         {"1.2"},
	})
	card := Card{FieldNote: {zh}}

	got := card.Select(FieldNote, WithLanguages("zh"))
	if got != zh {
		t.Fatalf("expected same field pointer, got %+v", got)
	}
	if got.Params.Get("X-CUSTOM") != "kept" || got.Params.Get("PID") != "1.2" {
		t.Fatalf("unknown parameters must be preserved, got %+v", got.Params)
	}
}

func TestSelector_ZeroValueNoLanguageFallback(t *testing.T) {
	en := fieldWith("en", Params{ParamLanguage: {"en"}, ParamAltID: {"1"}})
	untagged := fieldWith("plain", Params{})
	card := Card{FieldNote: {en, untagged}}

	// Without options, language is not considered: both fields tie.
	if got := card.SelectAll(FieldNote); len(got) != 2 {
		t.Fatalf("expected both fields eligible with zero-value selector, got %+v", got)
	}

	// Directly using a strict Selector with a language preference must not
	// silently fall back to untagged or other languages.
	strict := Selector{Languages: []string{"zh"}}
	if got := card.SelectAll(FieldNote, func(s *Selector) { *s = strict }); got != nil {
		t.Fatalf("expected no match with strict selector, got %+v", got)
	}
}
