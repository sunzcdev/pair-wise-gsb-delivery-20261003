package vcard

import "strconv"

// A Selector configures which variant of a property Card.Select returns.
//
// Selection happens in order:
//
//  1. Fields sharing a non-empty ALTID parameter are treated as language
//     variants of a single logical property. Fields without ALTID each form
//     their own logical property. Structured properties such as N or ADR are
//     always selected as a whole field; components from different variants
//     are never mixed.
//  2. Within each ALTID group, the ordered Languages list selects the best
//     language variant using BCP 47 basic filtering (RFC 4647 section 3.3.1)
//     with explicit fallback.
//  3. Surviving properties are ranked by the ordered Types list (purpose such
//     as "work" or "home") and then by PREF.
//
// Field parameters are never copied or modified: selected fields are returned
// as stored on the card, so unknown parameters are preserved.
//
// Pass the option functions to Card.Select or Card.SelectAll, or build a
// Selector directly: the zero value selects fields without language fallback.
type Selector struct {
	// Languages is the ordered list of preferred language ranges, most
	// preferred first. Entries are BCP 47 basic language ranges: for example
	// "zh" matches both "zh-Hans" and "zh-Hant", while "zh-Hans" matches
	// "zh-Hans-CN" but not the bare tag "zh". The wildcard "*" matches any
	// language-tagged field. When empty, the field language does not affect
	// selection.
	Languages []string

	// Types is the ordered list of preferred TYPE values, most preferred
	// first, for example []string{TypeWork, TypeHome}. Matching is
	// case-insensitive. Fields without any listed type rank after matching
	// fields but are still eligible.
	Types []string

	// FallbackUntagged makes variants without a LANGUAGE parameter eligible
	// after every explicitly preferred language has been tried. Selectors
	// built with NewSelector enable this; the zero value does not.
	FallbackUntagged bool

	// FallbackAnyLanguage makes variants tagged with a non-matching language
	// eligible as the last resort, after untagged variants. The zero value
	// leaves such variants ineligible, which preserves an explicit "no match"
	// state.
	FallbackAnyLanguage bool

	// Tiebreaker chooses one field from a fully tied candidate set in
	// Card.Select. It receives the candidates in card (document) order and
	// must return one of them. When nil, the first candidate is returned.
	// Card.SelectAll ignores it and returns the whole tied set.
	Tiebreaker func(candidates []*Field) *Field
}

// SelectOption configures a Selector.
type SelectOption func(*Selector)

// NewSelector builds a Selector with sensible defaults: variants without a
// LANGUAGE parameter are used as a fallback when no preferred language is
// available, while variants in other languages are not.
func NewSelector(options ...SelectOption) Selector {
	sel := Selector{FallbackUntagged: true}
	for _, option := range options {
		option(&sel)
	}
	return sel
}

// WithLanguages sets the ordered preferred language ranges, most preferred
// first.
func WithLanguages(languages ...string) SelectOption {
	return func(sel *Selector) { sel.Languages = languages }
}

// WithTypes sets the ordered preferred TYPE values, most preferred first.
func WithTypes(types ...string) SelectOption {
	return func(sel *Selector) { sel.Types = types }
}

// WithUntaggedFallback toggles fallback to variants without a LANGUAGE
// parameter.
func WithUntaggedFallback(enabled bool) SelectOption {
	return func(sel *Selector) { sel.FallbackUntagged = enabled }
}

// WithAnyLanguageFallback toggles last-resort fallback to variants tagged with
// a non-matching language. Enabling it prevents Card.Select from reporting "no
// match" as long as the property exists.
func WithAnyLanguageFallback(enabled bool) SelectOption {
	return func(sel *Selector) { sel.FallbackAnyLanguage = enabled }
}

// WithTiebreaker sets the stable rule used to break fully tied candidates.
func WithTiebreaker(fn func(candidates []*Field) *Field) SelectOption {
	return func(sel *Selector) { sel.Tiebreaker = fn }
}

// Select returns the field of property key best matching the selector. It
// returns nil when the property is absent or no variant satisfies the
// configured language preference.
func (c Card) Select(key string, options ...SelectOption) *Field {
	candidates := c.SelectAll(key, options...)
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	sel := NewSelector(options...)
	if sel.Tiebreaker != nil {
		if chosen := sel.Tiebreaker(candidates); chosen != nil {
			return chosen
		}
	}
	return candidates[0]
}

// SelectValue behaves like Select but returns the selected field value, or the
// empty string when nothing matches.
func (c Card) SelectValue(key string, options ...SelectOption) string {
	field := c.Select(key, options...)
	if field == nil {
		return ""
	}
	return field.Value
}

// SelectAll returns every fully tied field of property key best matching the
// selector, in card (document) order. It returns nil when the property is
// absent or no variant satisfies the configured language preference.
func (c Card) SelectAll(key string, options ...SelectOption) []*Field {
	sel := NewSelector(options...)
	fields := c[key]
	if len(fields) == 0 {
		return nil
	}

	ranges := parseLanguageRanges(sel.Languages)

	// Group ALTID variants together; fields without ALTID are singletons.
	groupOrder := make([]string, 0, len(fields))
	groups := make(map[string][]*Field)
	for i, field := range fields {
		altid := field.Params.Get(ParamAltID)
		if altid == "" {
			altid = "\x00singleton\x00" + strconv.Itoa(i)
		}
		if _, ok := groups[altid]; !ok {
			groupOrder = append(groupOrder, altid)
		}
		groups[altid] = append(groups[altid], field)
	}

	// Stage 1: keep only the best language variant(s) of each logical field.
	type candidate struct{ field *Field }
	var survivors []candidate
	for _, altid := range groupOrder {
		variants := groups[altid]
		best := langRank{}
		haveBest := false
		for _, variant := range variants {
			rank, ok := languageRank(variant, ranges, sel)
			if !ok {
				continue
			}
			if !haveBest || rank.less(best) {
				best = rank
				haveBest = true
			}
		}
		if !haveBest {
			continue
		}
		for _, variant := range variants {
			rank, ok := languageRank(variant, ranges, sel)
			if ok && rank == best {
				survivors = append(survivors, candidate{variant})
			}
		}
	}
	if len(survivors) == 0 {
		return nil
	}

	// Stage 2: rank surviving variants by TYPE purpose then PREF.
	sortKey := func(field *Field) [2]int {
		return [2]int{typeRank(field, sel.Types), prefRank(field)}
	}

	best := sortKey(survivors[0].field)
	for _, candidate := range survivors[1:] {
		key := sortKey(candidate.field)
		if key[0] < best[0] || (key[0] == best[0] && key[1] < best[1]) {
			best = key
		}
	}

	matched := make([]*Field, 0, len(survivors))
	for _, candidate := range survivors {
		if sortKey(candidate.field) == best {
			matched = append(matched, candidate.field)
		}
	}
	return matched
}

// langRank orders language variants. preference is the matched language
// preference position (with fallback levels after it). exact ranks a tag
// equal to the matched range above partial subtree matches; different partial
// matches stay tied so SelectAll can return the candidate set.
type langRank struct {
	preference int
	exact      int
}

func (a langRank) less(b langRank) bool {
	if a.preference != b.preference {
		return a.preference < b.preference
	}
	return a.exact > b.exact
}

func languageRank(field *Field, ranges [][]string, sel Selector) (langRank, bool) {
	tag := field.Params.Get(ParamLanguage)
	tagSubtags := languageSubtags(tag)

	// Without a configured preference, language is not part of the ordering.
	if len(ranges) == 0 {
		return langRank{}, true
	}

	if tag != "" {
		for i, languageRange := range ranges {
			if basicLanguageFilter(languageRange, tagSubtags) {
				exact := 0
				if len(tagSubtags) == len(languageRange) {
					exact = 1
				}
				return langRank{
					preference: i,
					exact:      exact,
				}, true
			}
		}
		if sel.FallbackAnyLanguage {
			return langRank{preference: len(ranges) + 1}, true
		}
		return langRank{}, false
	}

	if sel.FallbackUntagged {
		return langRank{preference: len(ranges)}, true
	}
	return langRank{}, false
}

func typeRank(field *Field, preferred []string) int {
	for i, wanted := range preferred {
		if field.Params.HasType(wanted) {
			return i
		}
	}
	return len(preferred)
}

func prefRank(field *Field) int {
	if pref := field.Params.Get(ParamPreferred); pref != "" {
		if n, err := strconv.Atoi(pref); err == nil {
			return n
		}
	}
	if field.Params.HasType("pref") {
		// Apple Contacts adds "pref" to the TYPE param.
		return 1
	}
	return 100
}

func parseLanguageRanges(languages []string) [][]string {
	if len(languages) == 0 {
		return nil
	}
	ranges := make([][]string, 0, len(languages))
	for _, language := range languages {
		subtags := languageSubtags(language)
		if len(subtags) > 0 {
			ranges = append(ranges, subtags)
		}
	}
	return ranges
}

func languageSubtags(tag string) []string {
	parts := splitTag(tag)
	subtags := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			subtags = append(subtags, part)
		}
	}
	return subtags
}

func splitTag(tag string) []string {
	count := 1
	for i := 0; i < len(tag); i++ {
		if tag[i] == '-' {
			count++
		}
	}
	parts := make([]string, 0, count)
	start := 0
	for i := 0; i < len(tag); i++ {
		if tag[i] == '-' {
			parts = append(parts, lower(tag[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, lower(tag[start:]))
	return parts
}

func lower(s string) string {
	bytes := []byte(s)
	for i, b := range bytes {
		if b >= 'A' && b <= 'Z' {
			bytes[i] = b + ('a' - 'A')
		}
	}
	return string(bytes)
}

// basicLanguageFilter implements the BCP 47 basic filtering scheme from
// RFC 4647 section 3.3.1.
func basicLanguageFilter(languageRange, tag []string) bool {
	pointer := 0
	for _, rangeSubtag := range languageRange {
		if pointer >= len(tag) {
			return false
		}
		if rangeSubtag == "*" {
			pointer++
			continue
		}
		matched := false
		for ; pointer < len(tag); pointer++ {
			if tag[pointer] == rangeSubtag {
				matched = true
				pointer++
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
