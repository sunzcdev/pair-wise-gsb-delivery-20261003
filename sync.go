package vcard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A PID is a parsed PID parameter value, defined in RFC 6350 section 5.5.
// It identifies a property instance in the namespace of a synchronization
// client. Source refers to the source ID of a CLIENTPIDMAP field, ID is
// assigned by the client itself.
type PID struct {
	Source int
	ID     string
}

// ParsePID parses a PID parameter value, e.g. "1.1".
func ParsePID(s string) (PID, error) {
	i := strings.Index(s, ".")
	if i <= 0 || i == len(s)-1 {
		return PID{}, fmt.Errorf("vcard: malformed PID %q", s)
	}
	source, err := strconv.Atoi(s[:i])
	if err != nil {
		return PID{}, fmt.Errorf("vcard: malformed PID source ID %q", s[:i])
	}
	return PID{Source: source, ID: s[i+1:]}, nil
}

// String formats the PID as a PID parameter value.
func (pid PID) String() string {
	return strconv.Itoa(pid.Source) + "." + pid.ID
}

// PIDs parses the PID parameters of a field. A field can carry multiple
// PIDs when it is known to multiple synchronization clients.
func (f *Field) PIDs() ([]PID, error) {
	values := f.Params[ParamPID]
	if len(values) == 0 {
		return nil, nil
	}
	pids := make([]PID, len(values))
	for i, v := range values {
		pid, err := ParsePID(v)
		if err != nil {
			return nil, err
		}
		pids[i] = pid
	}
	return pids, nil
}

// SetPIDs replaces the PID parameters of a field.
func (f *Field) SetPIDs(pids []PID) {
	if len(pids) == 0 {
		delete(f.Params, ParamPID)
		return
	}
	if f.Params == nil {
		f.Params = make(Params)
	}
	values := make([]string, len(pids))
	for i, pid := range pids {
		values[i] = pid.String()
	}
	f.Params[ParamPID] = values
}

// A ClientPIDMap is a parsed CLIENTPIDMAP field, defined in RFC 6350
// section 6.7.7. It maps a local source ID, as used in PID parameters, to
// the global URI of a synchronization client.
type ClientPIDMap struct {
	Source int
	URI    string
}

// ParseClientPIDMap parses a CLIENTPIDMAP field value, e.g.
// "1;urn:uuid:53e374d9-337e-4727-8803-a1e9c14e0556".
func ParseClientPIDMap(v string) (ClientPIDMap, error) {
	i := strings.Index(v, ";")
	if i <= 0 {
		return ClientPIDMap{}, fmt.Errorf("vcard: malformed CLIENTPIDMAP %q", v)
	}
	source, err := strconv.Atoi(v[:i])
	if err != nil {
		return ClientPIDMap{}, fmt.Errorf("vcard: malformed CLIENTPIDMAP source ID %q", v[:i])
	}
	return ClientPIDMap{Source: source, URI: v[i+1:]}, nil
}

// String formats the map as a CLIENTPIDMAP field value.
func (m ClientPIDMap) String() string {
	return strconv.Itoa(m.Source) + ";" + m.URI
}

// ClientPIDMaps parses the CLIENTPIDMAP fields of a card.
func (c Card) ClientPIDMaps() ([]ClientPIDMap, error) {
	fields := c[FieldClientPIDMap]
	if len(fields) == 0 {
		return nil, nil
	}
	maps := make([]ClientPIDMap, len(fields))
	for i, f := range fields {
		m, err := ParseClientPIDMap(f.Value)
		if err != nil {
			return nil, err
		}
		maps[i] = m
	}
	return maps, nil
}

// A GlobalPID is a PID normalized into the global namespace: the local
// source ID has been resolved to the synchronization client URI via the
// card's CLIENTPIDMAP fields. Two PIDs from different cards refer to the
// same property instance if and only if their GlobalPIDs are equal.
type GlobalPID struct {
	Client string
	ID     string
}

// A MissingClientPIDMapError is returned when a PID parameter refers to a
// source ID which has no matching CLIENTPIDMAP field.
type MissingClientPIDMapError struct {
	Source int
}

func (err *MissingClientPIDMapError) Error() string {
	return fmt.Sprintf("vcard: no CLIENTPIDMAP field for source ID %d", err.Source)
}

// GlobalPIDs resolves the PID parameters of a field into the global
// namespace, using the card's CLIENTPIDMAP fields.
func (c Card) GlobalPIDs(f *Field) ([]GlobalPID, error) {
	pids, err := f.PIDs()
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, nil
	}
	maps, err := c.ClientPIDMaps()
	if err != nil {
		return nil, err
	}
	clients := make(map[int]string, len(maps))
	for _, m := range maps {
		clients[m.Source] = m.URI
	}
	global := make([]GlobalPID, len(pids))
	for i, pid := range pids {
		client, ok := clients[pid.Source]
		if !ok {
			return nil, &MissingClientPIDMapError{Source: pid.Source}
		}
		global[i] = GlobalPID{Client: client, ID: pid.ID}
	}
	return global, nil
}

// A MergeConflict describes a pair of fields which share a global PID --
// i.e. both synchronization clients agree that they are the same property
// instance -- but carry different contents. MergeCards keeps the field
// from the first card in the merged result; the caller decides which
// version to keep.
type MergeConflict struct {
	PID    GlobalPID
	First  *Field
	Second *Field
}

type mergeEntry struct {
	name  string
	field *Field
	gpids []GlobalPID
}

// MergeCards merges the fields of two cards into a new card.
//
// Fields are matched by synchronization identity, not by contents: two
// fields are considered the same property instance only if they share a
// global PID (see GlobalPIDs). Fields with identical contents but
// different identities are kept separate. Fields without any PID are
// copied, collapsing exact duplicates (e.g. VERSION).
//
// The merged card contains fresh CLIENTPIDMAP fields and PID parameters
// renumbered so that every reference resolves to the correct client.
//
// If the same global PID is attached to fields with different contents, a
// MergeConflict is reported and the field from the first card is used in
// the merged card.
func MergeCards(first, second Card) (Card, []MergeConflict, error) {
	// Assign a fresh local source ID to each client URI, in order of
	// appearance.
	clientIDs := make(map[string]int)
	var clients []string
	sourceMaps := make([]map[int]string, 2)
	for i, c := range []Card{first, second} {
		maps, err := c.ClientPIDMaps()
		if err != nil {
			return nil, nil, err
		}
		bySource := make(map[int]string, len(maps))
		for _, m := range maps {
			bySource[m.Source] = m.URI
			if _, ok := clientIDs[m.URI]; !ok {
				clientIDs[m.URI] = len(clients) + 1
				clients = append(clients, m.URI)
			}
		}
		sourceMaps[i] = bySource
	}

	var entries []*mergeEntry
	index := make(map[GlobalPID]*mergeEntry)
	var conflicts []MergeConflict

	resolve := func(cardIndex int, f *Field) ([]GlobalPID, error) {
		pids, err := f.PIDs()
		if err != nil {
			return nil, err
		}
		global := make([]GlobalPID, len(pids))
		for i, pid := range pids {
			client, ok := sourceMaps[cardIndex][pid.Source]
			if !ok {
				return nil, &MissingClientPIDMapError{Source: pid.Source}
			}
			global[i] = GlobalPID{Client: client, ID: pid.ID}
		}
		return global, nil
	}

	// attachNew registers gpids not yet known to any entry on e.
	attachNew := func(e *mergeEntry, gpids []GlobalPID) {
		for _, g := range gpids {
			if _, ok := index[g]; ok {
				continue
			}
			index[g] = e
			e.gpids = append(e.gpids, g)
		}
	}

	// absorb moves all of src's global PIDs to dst and removes src.
	absorb := func(dst, src *mergeEntry) {
		for _, g := range src.gpids {
			index[g] = dst
			dst.gpids = append(dst.gpids, g)
		}
		for i, e := range entries {
			if e == src {
				entries = append(entries[:i], entries[i+1:]...)
				break
			}
		}
	}

	for cardIndex, c := range []Card{first, second} {
		for _, name := range sortedCardKeys(c) {
			if name == FieldClientPIDMap {
				continue
			}
			for _, f := range c[name] {
				gpids, err := resolve(cardIndex, f)
				if err != nil {
					return nil, nil, err
				}

				if len(gpids) == 0 {
					duplicate := false
					for _, e := range entries {
						if e.name == name && len(e.gpids) == 0 && fieldsEquivalent(e.field, f) {
							duplicate = true
							break
						}
					}
					if !duplicate {
						entries = append(entries, &mergeEntry{name: name, field: f})
					}
					continue
				}

				// Find existing entries this field refers to.
				var target *mergeEntry
				for _, g := range gpids {
					e := index[g]
					if e == nil || e == target {
						continue
					}
					if target == nil {
						target = e
						continue
					}
					// The incoming field bridges two entries: they
					// are the same property instance.
					if fieldsEquivalent(target.field, e.field) && fieldsEquivalent(target.field, f) {
						absorb(target, e)
					} else {
						conflicts = append(conflicts, MergeConflict{PID: g, First: target.field, Second: e.field})
					}
				}

				if target == nil {
					target = &mergeEntry{name: name, field: f}
					entries = append(entries, target)
					attachNew(target, gpids)
					continue
				}

				if !fieldsEquivalent(target.field, f) {
					for _, g := range gpids {
						if index[g] == target {
							conflicts = append(conflicts, MergeConflict{PID: g, First: target.field, Second: f})
						}
					}
				}
				attachNew(target, gpids)
			}
		}
	}

	merged := make(Card)
	for i, uri := range clients {
		merged.Add(FieldClientPIDMap, &Field{
			Value: ClientPIDMap{Source: i + 1, URI: uri}.String(),
		})
	}
	for _, e := range entries {
		f := copyField(e.field)
		if len(e.gpids) > 0 {
			pids := make([]PID, len(e.gpids))
			for i, g := range e.gpids {
				pids[i] = PID{Source: clientIDs[g.Client], ID: g.ID}
			}
			f.SetPIDs(pids)
		}
		merged.Add(e.name, f)
	}
	return merged, conflicts, nil
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
	copied := *f
	if f.Params != nil {
		copied.Params = make(Params, len(f.Params))
		for k, values := range f.Params {
			copied.Params[k] = append([]string(nil), values...)
		}
	}
	return &copied
}

// fieldsEquivalent reports whether two fields carry the same contents,
// ignoring PID parameters.
func fieldsEquivalent(a, b *Field) bool {
	return a.Value == b.Value && a.Group == b.Group && paramsEquivalent(a.Params, b.Params)
}

func paramsEquivalent(a, b Params) bool {
	a = withoutPID(a)
	b = withoutPID(b)
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !stringSetsEqual(av, bv) {
			return false
		}
	}
	return true
}

func withoutPID(p Params) Params {
	filtered := make(Params, len(p))
	for k, v := range p {
		if k == ParamPID {
			continue
		}
		filtered[k] = v
	}
	return filtered
}

func stringSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
