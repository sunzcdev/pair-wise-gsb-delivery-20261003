package dns

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

var (
	// ErrSerialCompare indicates that two SOA serial numbers are
	// incomparable under RFC 1982 serial number arithmetic (their
	// difference is exactly 2^31).
	ErrSerialCompare error = &Error{err: "serial numbers are incomparable"}
	// ErrSerialOrder indicates that the new SOA serial is not after the
	// old SOA serial under RFC 1982 serial number arithmetic.
	ErrSerialOrder error = &Error{err: "new serial is not after old serial"}
	// ErrIXFRFormat indicates that an IXFR stream is malformed, e.g. a
	// section or the final SOA is missing.
	ErrIXFRFormat error = &Error{err: "malformed ixfr stream"}
	// ErrIXFRChain indicates that the SOA serials of consecutive IXFR
	// deltas do not chain.
	ErrIXFRChain error = &Error{err: "ixfr serial chain broken"}
)

// serialEpoch is 2^31, the epoch boundary of RFC 1982 serial arithmetic.
const serialEpoch = 1 << 31

// serialCompare compares a to b using RFC 1982 serial number arithmetic.
// It returns -1 if a is before b, 0 if they are equal and +1 if a is after
// b. If the two serials are exactly 2^31 apart the relation is undefined
// and ErrSerialCompare is returned.
func serialCompare(a, b uint32) (int, error) {
	if a == b {
		return 0, nil
	}
	if d := a - b; d < serialEpoch {
		return 1, nil
	} else if d > serialEpoch {
		return -1, nil
	}
	return 0, ErrSerialCompare
}

// ZoneStore is a minimal storage interface for the contents of a single
// zone. It is used by ApplyIXFR to read the local copy of a zone and to
// atomically replace it once a transfer has been fully validated.
type ZoneStore interface {
	// All returns the full contents of the zone as an RR stream with
	// the SOA record first.
	All() []RR
	// Replace atomically replaces the zone contents with rrs, which
	// starts with the zone's SOA record.
	Replace(rrs []RR)
}

// MemZoneStore is a simple in-memory ZoneStore.
type MemZoneStore struct {
	rrs []RR
}

// NewMemZoneStore returns a MemZoneStore holding rrs, which must start
// with the zone's SOA record.
func NewMemZoneStore(rrs []RR) *MemZoneStore { return &MemZoneStore{rrs: rrs} }

// All implements ZoneStore.
func (z *MemZoneStore) All() []RR {
	rrs := make([]RR, len(z.rrs))
	copy(rrs, z.rrs)
	return rrs
}

// Replace implements ZoneStore.
func (z *MemZoneStore) Replace(rrs []RR) { z.rrs = rrs }

// ixfrKey returns a key identifying an RR by owner name (case
// insensitively), class, type, TTL and rdata. Note that unlike IsDuplicate
// the TTL is part of the key: a TTL change is a real change.
func ixfrKey(rr RR) string {
	h := rr.Header()
	s, hs := rr.String(), h.String()
	rdata := s
	if strings.HasPrefix(s, hs) {
		rdata = s[len(hs):]
	}
	var sb strings.Builder
	sb.WriteString(strings.ToLower(h.Name))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(int(h.Class)))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(int(h.Rrtype)))
	sb.WriteByte(' ')
	sb.WriteString(strconv.FormatUint(uint64(h.Ttl), 10))
	sb.WriteByte(' ')
	sb.WriteString(rdata)
	return sb.String()
}

// ixfrSnapshot splits a zone snapshot into its SOA and a set of the
// remaining RRs. Duplicate RRs collapse into a single entry so they are
// never deleted or added more than once.
func ixfrSnapshot(zone []RR) (*SOA, map[string]RR, error) {
	if len(zone) == 0 {
		return nil, nil, ErrSoa
	}
	soa, ok := zone[0].(*SOA)
	if !ok {
		return nil, nil, ErrSoa
	}
	set := make(map[string]RR, len(zone))
	for _, rr := range zone[1:] {
		if rr == nil || rr.Header().Rrtype == TypeSOA {
			continue
		}
		set[ixfrKey(rr)] = rr
	}
	return soa, set, nil
}

// GenerateIXFR computes the difference between two zone snapshots and
// returns it as an IXFR RR stream as defined in RFC 1995 section 4:
//
//	SOA(new) SOA(old) <deleted RRs> SOA(new) <added RRs> SOA(new)
//
// Both old and new are complete zone contents with the SOA record first.
// The relation between the two SOA serials is determined with RFC 1982
// serial number arithmetic, so serial number wraparound is handled. If the
// serials are equal the zones are considered identical and a single SOA
// record is returned. If the new serial is not after the old serial, or
// the serials are incomparable, an error is returned.
//
// RRs are compared including their TTL and duplicate RRs in the input are
// collapsed, so each changed RR appears at most once in the output.
func GenerateIXFR(old, new []RR) ([]RR, error) {
	oldSoa, oldSet, err := ixfrSnapshot(old)
	if err != nil {
		return nil, err
	}
	newSoa, newSet, err := ixfrSnapshot(new)
	if err != nil {
		return nil, err
	}
	cmp, err := serialCompare(newSoa.Serial, oldSoa.Serial)
	if err != nil {
		return nil, err
	}
	switch cmp {
	case 0:
		return []RR{newSoa}, nil
	case -1:
		return nil, ErrSerialOrder
	}

	var deleted, added []RR
	for k, rr := range oldSet {
		if _, ok := newSet[k]; !ok {
			deleted = append(deleted, rr)
		}
	}
	for k, rr := range newSet {
		if _, ok := oldSet[k]; !ok {
			added = append(added, rr)
		}
	}
	// Sort for a deterministic output stream.
	sort.Slice(deleted, func(i, j int) bool { return ixfrKey(deleted[i]) < ixfrKey(deleted[j]) })
	sort.Slice(added, func(i, j int) bool { return ixfrKey(added[i]) < ixfrKey(added[j]) })

	out := make([]RR, 0, len(deleted)+len(added)+3)
	out = append(out, newSoa, oldSoa)
	out = append(out, deleted...)
	out = append(out, newSoa)
	out = append(out, added...)
	out = append(out, newSoa)
	return out, nil
}

// rrBag is an ordered set of RRs used to stage zone contents while
// applying an IXFR.
type rrBag struct {
	order []string
	set   map[string]RR
}

func newRRBag(capacity int) *rrBag {
	return &rrBag{set: make(map[string]RR, capacity)}
}

func (b *rrBag) add(rr RR) {
	k := ixfrKey(rr)
	if _, ok := b.set[k]; !ok {
		b.order = append(b.order, k)
	}
	b.set[k] = rr
}

// remove deletes rr from the bag. Removing an RR that is not present is
// not an error.
func (b *rrBag) remove(rr RR) {
	delete(b.set, ixfrKey(rr))
}

func (b *rrBag) rrs() []RR {
	out := make([]RR, 0, len(b.set))
	for _, k := range b.order {
		if rr, ok := b.set[k]; ok {
			out = append(out, rr)
		}
	}
	return out
}

// ApplyIXFR applies a received zone transfer to the local copy of the zone
// held in store. The stream of envelopes, as returned by Transfer.In, is
// flattened into a single RR stream which is then validated and applied:
//
//   - A stream of a single SOA record means the zone is up to date and is
//     a no-op.
//   - If the server fell back to AXFR (the second record is not an SOA
//     with an older serial) the whole zone is replaced.
//   - Otherwise the stream must be a sequence of one or more RFC 1995
//     deltas whose SOA serials chain from the serial of the local copy up
//     to the serial of the first SOA in the stream, terminated by a final
//     SOA with that same serial.
//
// The zone in store is only replaced when the entire stream has been
// received and validated; on any error, including cancellation of ctx, the
// store is left untouched.
func ApplyIXFR(ctx context.Context, store ZoneStore, ch <-chan *Envelope) error {
	var rrs []RR
loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-ch:
			if !ok {
				break loop
			}
			if e.Error != nil {
				return e.Error
			}
			rrs = append(rrs, e.RR...)
		}
	}
	return applyIXFR(store, rrs)
}

// applyIXFR validates the flattened transfer stream rrs and, only when the
// stream is complete and valid, replaces the contents of store.
func applyIXFR(store ZoneStore, rrs []RR) error {
	if len(rrs) == 0 {
		return ErrSoa
	}
	first, ok := rrs[0].(*SOA)
	if !ok {
		return ErrSoa
	}
	final := first.Serial
	if len(rrs) == 1 {
		// Up to date, nothing to do.
		return nil
	}

	if second, ok := rrs[1].(*SOA); !ok || second.Serial == final {
		// The server fell back to AXFR: SOA, records..., SOA.
		last, ok := rrs[len(rrs)-1].(*SOA)
		if !ok || last.Serial != final {
			return ErrIXFRFormat
		}
		store.Replace(rrs[:len(rrs)-1])
		return nil
	}

	cur := store.All()
	if len(cur) == 0 {
		return ErrSoa
	}
	curSoa, ok := cur[0].(*SOA)
	if !ok {
		return ErrSoa
	}

	bag := newRRBag(len(cur))
	for _, rr := range cur[1:] {
		if rr == nil || rr.Header().Rrtype == TypeSOA {
			continue
		}
		bag.add(rr)
	}

	// Walk the deltas: SOA(old) deletes... SOA(new) adds... repeated,
	// terminated by a final SOA(final).
	i := 1
	prevNew := curSoa.Serial
	for {
		if i >= len(rrs) {
			return ErrIXFRFormat // missing delta or final SOA
		}
		oldSoa, ok := rrs[i].(*SOA)
		if !ok {
			return ErrIXFRFormat
		}
		if oldSoa.Serial != prevNew {
			return ErrIXFRChain
		}
		i++
		for i < len(rrs) {
			if _, ok := rrs[i].(*SOA); ok {
				break
			}
			bag.remove(rrs[i])
			i++
		}
		if i >= len(rrs) {
			return ErrIXFRFormat
		}
		newSoa := rrs[i].(*SOA)
		cmp, err := serialCompare(newSoa.Serial, oldSoa.Serial)
		if err != nil {
			return err
		}
		if cmp <= 0 {
			return ErrIXFRChain
		}
		i++
		for i < len(rrs) {
			if _, ok := rrs[i].(*SOA); ok {
				break
			}
			bag.add(rrs[i])
			i++
		}
		prevNew = newSoa.Serial
		if prevNew != final {
			continue // more deltas must follow
		}
		// The stream must end with a final SOA carrying the same serial.
		if i >= len(rrs) {
			return ErrIXFRFormat
		}
		fin, ok := rrs[i].(*SOA)
		if !ok || fin.Serial != final {
			return ErrIXFRChain
		}
		if i != len(rrs)-1 {
			return ErrIXFRFormat
		}
		break
	}

	out := make([]RR, 0, len(bag.set)+1)
	out = append(out, first)
	out = append(out, bag.rrs()...)
	store.Replace(out)
	return nil
}
