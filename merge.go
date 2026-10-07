package vcard

import (
	"sort"
)

// A MergeVariant is one side of a merge conflict: a property name and its
// field as found in one of the merged cards.
type MergeVariant struct {
	Property string
	Field    *Field

	identities []GlobalPID
}

// A MergeConflict is returned when the same global identity (a GlobalPID)
// maps to fields with inconsistent contents in the merged cards. The caller
// decides which variant to keep and applies it with Merger.Resolve.
type MergeConflict struct {
	// Identity is the global identity shared by all variants.
	Identity GlobalPID
	// Variants holds the conflicting contents, in merge order.
	Variants []MergeVariant
}

type mergeGroup struct {
	property   string
	field      *Field // canonical content, without PID params
	identities []GlobalPID
	locals     []int // renumbered local-only PIDs
}

type mergedExtra struct {
	property string
	field    *Field
}

// A Merger merges cards that carry PID and CLIENTPIDMAP synchronization
// metadata into a single card with a normalized PID namespace.
//
// Fields are matched across cards by global identity only: compound PIDs are
// resolved to GlobalPIDs via each card's CLIENTPIDMAP, so two cards may use
// the same local numbers for different clients without colliding. Fields
// with identical contents but different identities are kept as separate
// sync objects. Fields sharing an identity with inconsistent contents are
// reported as conflicts. In the merged card, each client URI gets a fresh
// CLIENTPIDMAP source ID and local-only PIDs are renumbered so that no two
// fields share a local identifier.
type Merger struct {
	clients    []string
	clientIDs  map[string]int // client URI -> source ID (1-based)
	groups     []*mergeGroup
	byIdentity map[GlobalPID]*mergeGroup
	extras     []mergedExtra
	usedLocals map[int]struct{}
}

// NewMerger creates a new Merger.
func NewMerger() *Merger {
	return &Merger{
		clientIDs:  make(map[string]int),
		byIdentity: make(map[GlobalPID]*mergeGroup),
		usedLocals: make(map[int]struct{}),
	}
}

// Merge absorbs a card into the merger. It returns the conflicts detected
// while merging; conflicting fields are left out of the merged card until
// the caller resolves them with Resolve. An error is returned for malformed
// PID or CLIENTPIDMAP data, including PIDs referencing a CLIENTPIDMAP source
// the card doesn't define.
func (m *Merger) Merge(c Card) ([]MergeConflict, error) {
	maps, err := c.ClientPIDMaps()
	if err != nil {
		return nil, err
	}
	local := make(map[int]string, len(maps))
	for _, cm := range maps {
		local[cm.SourceID] = cm.URI
		if _, ok := m.clientIDs[cm.URI]; !ok {
			m.clients = append(m.clients, cm.URI)
			m.clientIDs[cm.URI] = len(m.clients)
		}
	}

	var conflicts []MergeConflict
	for _, prop := range sortedCardKeys(c) {
		if prop == FieldClientPIDMap {
			continue
		}
		for _, f := range c[prop] {
			conflict, err := m.mergeField(prop, f, local)
			if err != nil {
				return nil, err
			}
			conflicts = append(conflicts, conflict...)
		}
	}
	return conflicts, nil
}

func (m *Merger) mergeField(prop string, f *Field, local map[int]string) ([]MergeConflict, error) {
	pids, err := f.PIDs()
	if err != nil {
		return nil, err
	}

	var identities []GlobalPID
	seenIdentity := make(map[GlobalPID]bool)
	var locals []PID
	for _, pid := range pids {
		if pid.Source == 0 {
			locals = append(locals, pid)
			continue
		}
		uri, ok := local[pid.Source]
		if !ok {
			return nil, &MissingClientPIDMapError{SourceID: pid.Source}
		}
		id := GlobalPID{ClientURI: uri, LocalID: pid.ID}
		if !seenIdentity[id] {
			seenIdentity[id] = true
			identities = append(identities, id)
		}
	}

	if len(identities) == 0 {
		// No global identity: the field cannot be matched with fields from
		// other cards, keep it as-is with renumbered local PIDs.
		nf := copyField(f)
		if len(locals) > 0 {
			newPIDs := make([]PID, len(locals))
			for i, pid := range locals {
				newPIDs[i] = PID{ID: m.allocLocal(pid.ID)}
			}
			nf.SetPIDs(newPIDs)
		}
		m.extras = append(m.extras, mergedExtra{property: prop, field: nf})
		return nil, nil
	}

	// Find the existing groups referenced by the field's identities and
	// check content consistency.
	var matched []*mergeGroup
	var conflicts []MergeConflict
	for _, id := range identities {
		g, ok := m.byIdentity[id]
		if !ok || containsGroup(matched, g) {
			continue
		}
		if g.property == prop && contentEqual(g.field, f) {
			matched = append(matched, g)
		} else {
			conflicts = append(conflicts, MergeConflict{
				Identity: id,
				Variants: []MergeVariant{
					{Property: g.property, Field: copyField(g.field), identities: append([]GlobalPID(nil), g.identities...)},
					{Property: prop, Field: copyField(f), identities: append([]GlobalPID(nil), identities...)},
				},
			})
		}
	}
	if len(conflicts) > 0 {
		return conflicts, nil
	}

	if len(matched) == 0 {
		g := &mergeGroup{property: prop, field: stripPIDs(f)}
		m.groups = append(m.groups, g)
		matched = []*mergeGroup{g}
	}

	rep := matched[0]
	// A single field may bridge groups that were created separately; fold
	// them into the representative group.
	for _, g := range matched[1:] {
		for _, id := range g.identities {
			m.byIdentity[id] = rep
			rep.identities = append(rep.identities, id)
		}
		rep.locals = append(rep.locals, g.locals...)
		removeGroup(m, g)
	}
	for _, id := range identities {
		if _, ok := m.byIdentity[id]; !ok {
			rep.identities = append(rep.identities, id)
		}
		m.byIdentity[id] = rep
	}
	for _, pid := range locals {
		rep.locals = append(rep.locals, m.allocLocal(pid.ID))
	}
	return nil, nil
}

// Resolve applies the caller's choice for a conflict returned by Merge.
// The choice argument indexes conflict.Variants. The chosen variant replaces
// whatever its identities previously mapped to.
func (m *Merger) Resolve(c MergeConflict, choice int) {
	if choice < 0 || choice >= len(c.Variants) {
		return
	}
	v := c.Variants[choice]
	for _, id := range v.identities {
		if g, ok := m.byIdentity[id]; ok {
			g.removeIdentity(id)
		}
	}
	g := &mergeGroup{property: v.Property, field: stripPIDs(v.Field)}
	m.groups = append(m.groups, g)
	for _, id := range v.identities {
		g.identities = append(g.identities, id)
		m.byIdentity[id] = g
	}
}

// Card builds the merged card. It contains one CLIENTPIDMAP field per client
// URI seen while merging, and every PID parameter is rewritten to the new
// namespace so that each reference resolves back to the correct client.
func (m *Merger) Card() Card {
	c := make(Card)
	for i, uri := range m.clients {
		cm := ClientPIDMap{SourceID: i + 1, URI: uri}
		c.Add(FieldClientPIDMap, &Field{Value: cm.String()})
	}
	for _, g := range m.groups {
		if len(g.identities) == 0 && len(g.locals) == 0 {
			continue
		}
		f := copyField(g.field)
		pids := make([]PID, 0, len(g.identities)+len(g.locals))
		for _, id := range g.identities {
			pids = append(pids, PID{Source: m.clientIDs[id.ClientURI], ID: id.LocalID})
		}
		sort.Slice(pids, func(i, j int) bool {
			if pids[i].Source != pids[j].Source {
				return pids[i].Source < pids[j].Source
			}
			return pids[i].ID < pids[j].ID
		})
		locals := append([]int(nil), g.locals...)
		sort.Ints(locals)
		for _, id := range locals {
			pids = append(pids, PID{ID: id})
		}
		f.SetPIDs(pids)
		c.Add(g.property, f)
	}
	for _, e := range m.extras {
		c.Add(e.property, copyField(e.field))
	}
	return c
}

// MergeCards merges cards carrying PID and CLIENTPIDMAP synchronization
// metadata into a single card. See Merger for the matching rules. Conflicts
// are returned for the caller to resolve; they are left out of the merged
// card.
func MergeCards(cards ...Card) (Card, []MergeConflict, error) {
	m := NewMerger()
	var conflicts []MergeConflict
	for _, c := range cards {
		cs, err := m.Merge(c)
		if err != nil {
			return nil, nil, err
		}
		conflicts = append(conflicts, cs...)
	}
	return m.Card(), conflicts, nil
}

// allocLocal returns a local-only PID identifier that is unused in the
// merged card, preferring want if it is still free.
func (m *Merger) allocLocal(want int) int {
	if want > 0 {
		if _, used := m.usedLocals[want]; !used {
			m.usedLocals[want] = struct{}{}
			return want
		}
	}
	for id := 1; ; id++ {
		if _, used := m.usedLocals[id]; !used {
			m.usedLocals[id] = struct{}{}
			return id
		}
	}
}

func (g *mergeGroup) removeIdentity(id GlobalPID) {
	for i, other := range g.identities {
		if other == id {
			g.identities = append(g.identities[:i], g.identities[i+1:]...)
			return
		}
	}
}

func containsGroup(groups []*mergeGroup, g *mergeGroup) bool {
	for _, other := range groups {
		if other == g {
			return true
		}
	}
	return false
}

func removeGroup(m *Merger, g *mergeGroup) {
	for i, other := range m.groups {
		if other == g {
			m.groups = append(m.groups[:i], m.groups[i+1:]...)
			return
		}
	}
}

func sortedCardKeys(c Card) []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func copyField(f *Field) *Field {
	nf := &Field{Value: f.Value, Group: f.Group}
	if f.Params != nil {
		nf.Params = make(Params, len(f.Params))
		for k, values := range f.Params {
			nf.Params[k] = append([]string(nil), values...)
		}
	}
	return nf
}

func stripPIDs(f *Field) *Field {
	nf := copyField(f)
	delete(nf.Params, ParamPID)
	return nf
}

// contentEqual reports whether two fields have the same content, ignoring
// PID parameters.
func contentEqual(a, b *Field) bool {
	if a.Value != b.Value || a.Group != b.Group {
		return false
	}
	ap := withoutPIDs(a.Params)
	bp := withoutPIDs(b.Params)
	if len(ap) != len(bp) {
		return false
	}
	for k, av := range ap {
		bv, ok := bp[k]
		if !ok {
			return false
		}
		as := append([]string(nil), av...)
		bs := append([]string(nil), bv...)
		sort.Strings(as)
		sort.Strings(bs)
		for i := range as {
			if as[i] != bs[i] {
				return false
			}
		}
	}
	return true
}

func withoutPIDs(p Params) Params {
	out := make(Params, len(p))
	for k, values := range p {
		if k == ParamPID {
			continue
		}
		out[k] = values
	}
	return out
}
