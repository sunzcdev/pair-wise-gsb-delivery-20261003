package vcard

import (
	"reflect"
	"strings"
	"testing"
)

func adrField(value string, params Params) *Field {
	return &Field{Value: value, Params: params}
}

// A contact has Chinese and English translations of the same address under one
// ALTID. The structured ADR must be selected whole and in the preferred
// language, never assembled from components of different languages.
func TestSelector_AltIDLanguageVariants(t *testing.T) {
	zhAdr := adrField(";;中关村大街1号;北京;;100080;中国", Params{
		ParamLanguage: {"zh"},
		ParamAltID:    {"1"},
	})
	enAdr := adrField(";;1 Zhongguancun Street;Beijing;;100080;China", Params{
		ParamLanguage: {"en"},
		ParamAltID:    {"1"},
	})
	card := Card{FieldAddress: {zhAdr, enAdr}}

	zh := (&Selector{Languages: []string{"zh"}}).Select(card, FieldAddress)
	if got := zh.Field(); got != zhAdr {
		t.Fatalf("Chinese preference: got %q, want %q", got.Value, zhAdr.Value)
	}
	if zh.Match != LangExact || !zh.MatchedLanguage() || zh.Tied {
		t.Fatalf("Chinese preference: unexpected result %+v", zh)
	}

	en := (&Selector{Languages: []string{"en"}}).Select(card, FieldAddress)
	if got := en.Field(); got != enAdr {
		t.Fatalf("English preference: got %q, want %q", got.Value, enAdr.Value)
	}
	if en.Match != LangExact {
		t.Fatalf("English preference: Match = %v, want LangExact", en.Match)
	}

	// N and ADR stay atomic: every component of the chosen Address comes
	// from the same source field.
	address := newAddress(zh.Field())
	if address.Country != "中国" || address.Locality != "北京" {
		t.Fatalf("address mixes language variants: %+v", address)
	}
}

// With no preference for any available language, the result must preserve the
// "no match" state while still returning a usable fallback field.
func TestSelector_MissingPreferredLanguage(t *testing.T) {
	zhAdr := adrField(";;南京路;上海;;200000;中国", Params{
		ParamLanguage: {"zh"},
		ParamAltID:    {"a"},
	})
	enAdr := adrField(";;Nanjing Road;Shanghai;;200000;China", Params{
		ParamLanguage: {"en"},
		ParamAltID:    {"a"},
	})
	card := Card{FieldAddress: {zhAdr, enAdr}}

	res := (&Selector{Languages: []string{"fr"}}).Select(card, FieldAddress)
	if res.Field() == nil {
		t.Fatal("expected a fallback field, got nil")
	}
	if res.Match != LangNone || res.MatchedLanguage() {
		t.Fatalf("Match = %v, want LangNone", res.Match)
	}

	// A property the card does not have at all reports the same "no match"
	// state but yields no field.
	missing := (&Selector{Languages: []string{"fr"}}).Select(card, FieldTelephone)
	if missing.Field() != nil {
		t.Fatalf("absent property: got field %q, want nil", missing.Field().Value)
	}
	if missing.Match != LangNone || missing.MatchedLanguage() {
		t.Fatalf("absent property: Match = %v, want LangNone", missing.Match)
	}
}

// Several translations under one ALTID with identical PREF are co-equal only
// when language preference cannot distinguish them.
func TestSelector_AltIDMultipleTranslationsSamePref(t *testing.T) {
	zh := adrField(";;香港道;香港;;;中国香港", Params{
		ParamLanguage: {"zh-Hant"}, ParamAltID: {"x"}, ParamPreferred: {"1"},
	})
	zhSimple := adrField(";;香港道;香港;;;中国", Params{
		ParamLanguage: {"zh-Hans"}, ParamAltID: {"x"}, ParamPreferred: {"1"},
	})
	en := adrField(";;Hong Kong Road;Hong Kong;;;China", Params{
		ParamLanguage: {"en"}, ParamAltID: {"x"}, ParamPreferred: {"1"},
	})
	card := Card{FieldAddress: {zh, zhSimple, en}}

	// Language preference breaks the same-PREF tie.
	got := (&Selector{Languages: []string{"zh-Hant"}}).Select(card, FieldAddress)
	if got.Field() != zh || got.Match != LangExact || got.Tied {
		t.Fatalf("zh-Hant preference: unexpected result %+v", got)
	}

	// No configured preference: all variants are language-neutral comparisons
	// and share the same PREF, so all co-equal candidates are returned.
	all := (&Selector{}).Select(card, FieldAddress)
	if !all.Tied {
		t.Fatal("expected Tied for same-PREF variants")
	}
	want := []*Field{zh, zhSimple, en}
	if !reflect.DeepEqual(all.Fields, want) {
		t.Fatalf("co-equal variants = %v, want %v", all.Fields, want)
	}
	if all.Match != LangNeutral || all.MatchedLanguage() {
		t.Fatalf("neutral variants: Match = %v, MatchedLanguage = true", all.Match)
	}

	// The same tie collapsed by a caller-selected stable rule (card order).
	first := (&Selector{Ties: TieFirst}).Select(card, FieldAddress)
	if first.Field() != zh || len(first.Fields) != 1 || !first.Tied {
		t.Fatalf("TieFirst: unexpected result %+v", first)
	}

	// A preference for an unavailable language leaves all variants equal at
	// LangNone: the tie set survives but the "no match" state is preserved.
	none := (&Selector{Languages: []string{"de"}, Ties: TieFirst}).
		Select(card, FieldAddress)
	if none.Match != LangNone || none.MatchedLanguage() || !none.Tied {
		t.Fatalf("unavailable language: unexpected result %+v", none)
	}
}

func TestSelector_BCP47FilteringAndFallback(t *testing.T) {
	s := &Selector{Languages: []string{"zh-Hans"}}

	cases := []struct {
		tag  string
		want LangMatch
	}{
		{"zh-Hans", LangExact},
		{"zh-hans", LangExact},     // case-insensitive
		{"zh-Hans-CN", LangFilter}, // basic filtering
		{"zh-Hant", LangFallback},  // truncate "zh-Hans" -> "zh"
		{"zh", LangFallback},       // truncate "zh-Hans" -> "zh"
		{"en", LangNone},           // unrelated tag
		{"", LangNeutral},          // untagged
		{"zhx", LangNone},          // subtag identity, not prefix of text
	}
	for _, tc := range cases {
		if got := s.MatchLanguage(tc.tag); got != tc.want {
			t.Errorf("MatchLanguage(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}

	// Preference order beats match strength: "en" is listed first.
	ordered := (&Selector{Languages: []string{"en", "zh"}}).MatchLanguage("zh")
	if ordered != LangExact {
		t.Fatalf("ordered preference = %v, want LangExact", ordered)
	}

	// The wildcard range matches any tagged language via basic filtering.
	wild := (&Selector{Languages: []string{"*"}}).MatchLanguage("ja-JP")
	if wild != LangFilter {
		t.Fatalf("wildcard = %v, want LangFilter", wild)
	}

	// Fallback picks the truncated preference over an unrelated language.
	zhHant := adrField(";;道1號;台北;;;臺灣", Params{
		ParamLanguage: {"zh-Hant"}, ParamAltID: {"1"},
	})
	en := adrField(";;1 Road;Taipei;;;Taiwan", Params{
		ParamLanguage: {"en"}, ParamAltID: {"1"},
	})
	card := Card{FieldAddress: {zhHant, en}}
	res := (&Selector{Languages: []string{"zh-Hans"}}).Select(card, FieldAddress)
	if res.Field() != zhHant || res.Match != LangFallback {
		t.Fatalf("fallback selection = %+v", res)
	}
}

func TestSelector_TypePurposeAndPref(t *testing.T) {
	home := &Field{Value: "tel:+1-555-HOME", Params: Params{ParamType: {"home", "voice"}}}
	work := &Field{Value: "tel:+1-555-WORK", Params: Params{ParamType: {"work", "voice"}}}
	card := Card{FieldTelephone: {home, work}}

	res := (&Selector{Types: []string{"work"}}).Select(card, FieldTelephone)
	if res.Field() != work {
		t.Fatalf("TYPE preference: got %q, want work", res.Field().Value)
	}

	// PREF ranks after TYPE; a PREF=1 home number does not beat a work number.
	homePref := &Field{Value: "tel:+1-555-HOME", Params: Params{
		ParamType: {"home"}, ParamPreferred: {"1"},
	}}
	workDefault := &Field{Value: "tel:+1-555-WORK", Params: Params{ParamType: {"work"}}}
	card = Card{FieldTelephone: {homePref, workDefault}}
	res = (&Selector{Types: []string{"work"}}).Select(card, FieldTelephone)
	if res.Field() != workDefault {
		t.Fatalf("TYPE must outrank PREF: got %q", res.Field().Value)
	}

	// Same TYPE: PREF decides.
	card = Card{FieldTelephone: {homePref, home}}
	res = (&Selector{Types: []string{"home"}}).Select(card, FieldTelephone)
	if res.Field() != homePref {
		t.Fatalf("PREF: got %q, want PREF=1 field", res.Field().Value)
	}

	// Apple-style TYPE=pref is honored.
	apple := &Field{Value: "tel:+1-555-APPLE", Params: Params{ParamType: {"voice", "pref"}}}
	plain := &Field{Value: "tel:+1-555-PLAIN", Params: Params{ParamType: {"voice"}}}
	card = Card{FieldTelephone: {plain, apple}}
	res = (&Selector{}).Select(card, FieldTelephone)
	if res.Field() != apple {
		t.Fatalf("TYPE=pref: got %q, want apple", res.Field().Value)
	}
}

func TestSelector_UnknownParamsPreserved(t *testing.T) {
	f := &Field{
		Value:  "user@example.com",
		Params: Params{"X-CUSTOM": {"keep-me"}, ParamLanguage: {"en"}},
		Group:  "contact1",
	}
	card := Card{FieldEmail: {f}}

	res := (&Selector{Languages: []string{"en"}}).Select(card, FieldEmail)
	if res.Field() != f {
		t.Fatal("selector must return the original field pointer")
	}
	if got := f.Params.Get("X-CUSTOM"); got != "keep-me" || f.Group != "contact1" {
		t.Fatalf("unknown parameters lost: %+v", f)
	}
}

func TestSelector_DistinctGroupsDoNotCompeteByLanguage(t *testing.T) {
	// Two independent addresses (different ALTID): one home in Chinese, one
	// work in English. TYPE selects a whole group; it never mixes components.
	home := adrField(";;府前街1号;北京;;100000;中国", Params{
		ParamLanguage: {"zh"}, ParamAltID: {"home"}, ParamType: {"home"},
	})
	work := adrField(";;1 Main St;New York;NY;10001;USA", Params{
		ParamLanguage: {"en"}, ParamAltID: {"work"}, ParamType: {"work"},
	})
	card := Card{FieldAddress: {home, work}}

	res := (&Selector{
		Languages: []string{"zh"},
		Types:     []string{"work"},
	}).Select(card, FieldAddress)
	if res.Field() != work {
		t.Fatalf("TYPE should select the work group wholesale, got %q", res.Field().Value)
	}
	if a := newAddress(res.Field()); a.Country != "USA" || a.Locality != "New York" {
		t.Fatalf("structured property assembled from mixed variants: %+v", a)
	}
}

func TestSelector_EmptyAndNilCard(t *testing.T) {
	var nilCard Card
	if res := (&Selector{Languages: []string{"en"}}).Select(nilCard, FieldName); res.Field() != nil {
		t.Fatal("nil card: expected no field")
	}

	empty := Card{}
	res := (&Selector{}).Select(empty, FieldName)
	if res.Field() != nil || res.Match != LangNone {
		t.Fatalf("empty card: unexpected result %+v", res)
	}

	// Within one ALTID group, an untagged variant is the language-neutral
	// fallback and beats a tagged variant matching no preference.
	plain := &Field{Value: "plain", Params: Params{ParamAltID: {"note"}}}
	japanese := &Field{Value: "japanese", Params: Params{
		ParamLanguage: {"ja"}, ParamAltID: {"note"},
	}}
	card := Card{FieldNote: {japanese, plain}}
	res = (&Selector{Languages: []string{"en"}, Ties: TieFirst}).Select(card, FieldNote)
	if res.Field() != plain || res.Match != LangNeutral {
		t.Fatalf("neutral fallback = %+v", res)
	}

	// Distinct properties (no shared ALTID) with equal TYPE/PREF are co-equal
	// across groups: TieFirst applies the caller's stable rule (card order),
	// while TieReturnAll keeps both and preserves the no-match state.
	other := &Field{Value: "other", Params: Params{ParamLanguage: {"ja"}}}
	card = Card{FieldNote: {japanese, other}}
	all := (&Selector{Languages: []string{"en"}}).Select(card, FieldNote)
	if len(all.Fields) != 2 || !all.Tied || all.Match != LangNone {
		t.Fatalf("distinct-group tie set = %+v", all)
	}
	first := (&Selector{Languages: []string{"en"}, Ties: TieFirst}).Select(card, FieldNote)
	if len(first.Fields) != 1 || first.Field() != japanese || !first.Tied {
		t.Fatalf("distinct-group TieFirst = %+v", first)
	}
}

func TestSelector_DecodedCardEndToEnd(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		"BEGIN:VCARD",
		"VERSION:4.0",
		"FN;ALTID=1;LANGUAGE=zh:张三",
		"FN;ALTID=1;LANGUAGE=en:John Zhang",
		"N;ALTID=2;LANGUAGE=zh:张;三;;;",
		"N;ALTID=2;LANGUAGE=en:Zhang;John;;;",
		"ADR;ALTID=3;TYPE=home;LANGUAGE=zh:;;幸福路1号;上海;;200000;中国",
		"ADR;ALTID=3;TYPE=home;LANGUAGE=en:;;1 Happy Road;Shanghai;;200000;China",
		"TEL;TYPE=work,voice;PREF=2:tel:+86-21-WORK",
		"TEL;TYPE=home,voice;PREF=1:tel:+86-21-HOME",
		"END:VCARD",
		"",
	}, "\r\n"))

	card, err := NewDecoder(input).Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	sel := &Selector{Languages: []string{"zh"}, Types: []string{"work"}}

	if fn := sel.Select(card, FieldFormattedName).Field(); fn == nil || fn.Value != "张三" {
		t.Fatalf("FN selection = %v", fn)
	}

	name := sel.Select(card, FieldName)
	if name.Field().Value != "张;三;;;" {
		t.Fatalf("N selection = %q, want Chinese N whole", name.Field().Value)
	}
	if n := newName(name.Field()); n.FamilyName != "张" || n.GivenName != "三" {
		t.Fatalf("N components mixed across languages: %+v", n)
	}

	adr := sel.Select(card, FieldAddress)
	if adr.Field().Value != ";;幸福路1号;上海;;200000;中国" {
		t.Fatalf("ADR selection = %q", adr.Field().Value)
	}

	// TYPE=work is requested even though the home number has the better PREF.
	tel := sel.Select(card, FieldTelephone)
	if tel.Field().Value != "tel:+86-21-WORK" {
		t.Fatalf("TEL selection = %q, want work number (TYPE outranks PREF)", tel.Field().Value)
	}

	en := (&Selector{Languages: []string{"en"}, Types: []string{"home"}}).
		Select(card, FieldAddress)
	if en.Field().Value != ";;1 Happy Road;Shanghai;;200000;China" {
		t.Fatalf("English ADR = %q", en.Field().Value)
	}
}
