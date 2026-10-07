package vcard

import (
	"sort"
	"strconv"
	"strings"
)

// LangMatch describes how a selected field relates to the Selector's language
// preferences.
type LangMatch int

const (
	// LangExact means the field's LANGUAGE tag equals the preferred range.
	LangExact LangMatch = iota
	// LangFilter means the field's LANGUAGE tag starts with the complete
	// preferred range, as in RFC 4647 section 3.3.1 basic filtering (e.g. a
	// "zh" preference matches a "zh-Hans-CN" field).
	LangFilter
	// LangFallback means the field matches after truncating a preferred
	// language, as in RFC 4647 section 3.4 lookup fallback (e.g. a "zh-Hant"
	// preference falls back to a "zh" field).
	LangFallback
	// LangNeutral means the field carries no LANGUAGE tag, or no language
	// preference was configured, so no language choice was made.
	LangNeutral
	// LangNone means the property exists, but no variant matches a preferred
	// language. The returned fields are fallbacks only.
	LangNone
)

// MatchedLanguage reports whether an explicitly preferred language was
// honored. It is false for language-neutral fields and for LangNone results.
func (m LangMatch) MatchedLanguage() bool {
	return m == LangExact || m == LangFilter || m == LangFallback
}

// TieRule controls what happens when several candidates are fully co-equal
// after language, TYPE and PREF ranking.
type TieRule int

const (
	// TieReturnAll keeps every co-equal candidate in the result, in card
	// order. This is the zero value.
	TieReturnAll TieRule = iota
	// TieFirst deterministically keeps the first candidate in card order.
	TieFirst
)

// A Selector chooses among the fields of a property.
//
// Fields sharing an ALTID are language variants of the same logical property
// and are ranked against each other first: the ordered Languages preferences
// (BCP 47 basic filtering with lookup fallback), then the ordered TYPE
// purposes, then PREF. Only the best variant(s) of each ALTID group survive to
// compete with other groups. Structured properties such as N and ADR are
// therefore always selected whole: components from different language
// variants are never mixed.
type Selector struct {
	// Languages is the ordered list of preferred language ranges, using BCP
	// 47 syntax (e.g. "zh-Hans", "en"). Earlier entries are preferred; "*"
	// matches any tagged language. When empty, language selection is skipped.
	Languages []string
	// Types is the ordered list of preferred TYPE purposes (e.g. "work",
	// "home"). Comparison is case-insensitive; earlier entries are
	// preferred. When empty, TYPE is not used for ranking.
	Types []string
	// Ties decides how fully co-equal candidates are handled.
	Ties TieRule
}

// A SelectResult is the outcome of Select.
type SelectResult struct {
	// Fields holds the selected fields, in card order. With TieReturnAll it
	// may contain several co-equal candidates; with TieFirst it contains at
	// most one. The pointers are the original card fields, so all other
	// parameters are preserved.
	Fields []*Field
	// Match reports the language match quality. With multiple fields it is
	// the worst quality among them, so callers can detect "no match" even
	// when fallback fields are returned.
	Match LangMatch
	// Tied reports that more than one fully co-equal candidate existed,
	// including when TieFirst collapsed the set to a single field.
	Tied bool
}

type candidate struct {
	field    *Field
	index    int
	match    LangMatch
	langRank int
	typeRank int
	prefRank int
}

// Field returns the first selected field, or nil if the property is absent.
func (r *SelectResult) Field() *Field {
	if len(r.Fields) == 0 {
		return nil
	}
	return r.Fields[0]
}

// MatchedLanguage reports whether every selected field honors a preferred
// language.
func (r *SelectResult) MatchedLanguage() bool {
	return r != nil && r.Field() != nil && r.Match.MatchedLanguage()
}

// Select chooses the fields of the given property from the card.
func (s *Selector) Select(card Card, key string) *SelectResult {
	fields := card[key]
	if len(fields) == 0 {
		return &SelectResult{Match: LangNone}
	}

	// Group fields by ALTID. Fields without an ALTID form singleton groups.
	// First-seen order is preserved.
	groups := make([][]*Field, 0, len(fields))
	altidIndex := make(map[string]int)
	for _, f := range fields {
		if altid := f.Params.Get(ParamAltID); altid != "" {
			if gi, ok := altidIndex[altid]; ok {
				groups[gi] = append(groups[gi], f)
				continue
			}
			altidIndex[altid] = len(groups)
		}
		groups = append(groups, []*Field{f})
	}

	// Best variant(s) of each ALTID group become representatives.
	var reps []candidate
	for _, group := range groups {
		cs := make([]candidate, len(group))
		for i, f := range group {
			match, langRank := s.languageScore(f.Params.Get(ParamLanguage))
			cs[i] = candidate{
				field:    f,
				index:    fieldIndex(fields, f),
				match:    match,
				langRank: langRank,
				typeRank: s.typeScore(f),
				prefRank: prefScore(f),
			}
		}

		sort.SliceStable(cs, lessCandidate(cs))

		best := cs[0]
		reps = append(reps, best)
		for _, c := range cs[1:] {
			if sameVariant(c, best) {
				reps = append(reps, c)
			} else {
				break
			}
		}
	}

	// Representatives compete across groups on TYPE purpose, then PREF.
	bestType, bestPref := reps[0].typeRank, reps[0].prefRank
	for _, r := range reps[1:] {
		if r.typeRank < bestType || (r.typeRank == bestType && r.prefRank < bestPref) {
			bestType, bestPref = r.typeRank, r.prefRank
		}
	}

	winners := reps[:0:0]
	worst := LangExact
	for _, r := range reps {
		if r.typeRank == bestType && r.prefRank == bestPref {
			winners = append(winners, r)
			if r.match > worst {
				worst = r.match
			}
		}
	}

	result := &SelectResult{Match: worst, Tied: len(winners) > 1}
	if s.Ties == TieFirst {
		result.Fields = []*Field{winners[0].field}
	} else {
		result.Fields = make([]*Field, len(winners))
		for i, w := range winners {
			result.Fields[i] = w.field
		}
	}
	return result
}

// MatchLanguage reports how a LANGUAGE tag matches the selector's preferences,
// without involving any card. An empty tag is language-neutral.
func (s *Selector) MatchLanguage(tag string) LangMatch {
	match, _ := s.languageScore(tag)
	return match
}

func (s *Selector) languageScore(tag string) (LangMatch, int) {
	tagParts := parseLanguageTag(tag)
	noPreference := len(s.Languages)

	if noPreference == 0 {
		return LangNeutral, 0
	}
	if len(tagParts) == 0 {
		// Language-neutral data is preferred over tagged data that matches
		// nothing, but never over an actual match.
		return LangNeutral, noPreference
	}

	// Full-range basic filtering.
	for i, pref := range s.Languages {
		rangeParts := parseLanguageTag(pref)
		if len(rangeParts) == 0 {
			continue
		}
		if len(rangeParts) == 1 && rangeParts[0] == "*" {
			return LangFilter, i
		}
		if basicFilterMatch(tagParts, rangeParts) {
			if len(tagParts) == len(rangeParts) {
				return LangExact, i
			}
			return LangFilter, i
		}
	}

	// Lookup fallback: progressively truncate the preferred range.
	for i, pref := range s.Languages {
		rangeParts := parseLanguageTag(pref)
		if len(rangeParts) == 0 || (len(rangeParts) == 1 && rangeParts[0] == "*") {
			continue
		}
		if lookupFallbackMatch(tagParts, rangeParts) {
			return LangFallback, i
		}
	}

	return LangNone, noPreference
}

func (s *Selector) typeScore(f *Field) int {
	for i, wanted := range s.Types {
		if f.Params.HasType(wanted) {
			return i
		}
	}
	return len(s.Types)
}

func prefScore(f *Field) int {
	if raw := f.Params.Get(ParamPreferred); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 {
			return n
		}
	}
	// Apple Contacts encodes preference with TYPE=pref.
	if f.Params.HasType("pref") {
		return 1
	}
	return 100
}

func lessCandidate(cs []candidate) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.match != b.match {
			return a.match < b.match
		}
		if a.langRank != b.langRank {
			return a.langRank < b.langRank
		}
		if a.typeRank != b.typeRank {
			return a.typeRank < b.typeRank
		}
		if a.prefRank != b.prefRank {
			return a.prefRank < b.prefRank
		}
		return a.index < b.index
	}
}

func sameVariant(a, b candidate) bool {
	return a.match == b.match &&
		a.langRank == b.langRank &&
		a.typeRank == b.typeRank &&
		a.prefRank == b.prefRank
}

func fieldIndex(fields []*Field, target *Field) int {
	for i, f := range fields {
		if f == target {
			return i
		}
	}
	return 0
}

// parseLanguageTag normalizes a BCP 47 tag/range into lowercase subtags.
func parseLanguageTag(tag string) []string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil
	}
	return strings.Split(strings.ToLower(tag), "-")
}

// basicFilterMatch implements RFC 4647 section 3.3.1: the range matches when
// its subtags equal the leading subtags of the tag. A "*" subtag matches any
// single subtag.
func basicFilterMatch(tag, languageRange []string) bool {
	if len(tag) < len(languageRange) {
		return false
	}
	for i, subtag := range languageRange {
		if subtag != "*" && subtag != tag[i] {
			return false
		}
	}
	return true
}

// lookupFallbackMatch implements RFC 4647 section 3.4: the range is
// progressively truncated, dropping singleton subtags along the way, until a
// prefix of the tag matches.
func lookupFallbackMatch(tag, languageRange []string) bool {
	tried := append([]string(nil), languageRange...)
	for len(tried) > 0 {
		if basicFilterMatch(tag, tried) {
			return true
		}
		// RFC 4647 section 3.4 step 4: truncate, and if the new tail is a
		// singleton subtag (or the removed tail was one), drop it too.
		tried = tried[:len(tried)-1]
		if len(tried) > 0 && len(tried[len(tried)-1]) == 1 {
			tried = tried[:len(tried)-1]
		}
	}
	return false
}
