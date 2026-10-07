package mysql

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// This file implements backfill (re-collection) planning for disaster
// recovery tools. Given three GTID sets — the locally completed GTIDs, the
// source's executed GTIDs and the source's purged GTIDs — the planner
// computes, before any connection to the source is made:
//
//   - what can still be fetched from the source (pending intervals),
//   - what the source is missing compared to the local side (divergence),
//   - which needed intervals have already been purged on the source
//     (unrecoverable gaps).
//
// MySQL and MariaDB are planned by separate functions on purpose: a MySQL
// GTID set is a real set of intervals keyed by (SID, tag), while a MariaDB
// "set" is only a high-watermark position per domain, so ranges that cannot
// be proven from a high watermark are reported as indeterminate instead of
// being forced into the same set model.
//
// All computations are interval based; individual transaction numbers are
// never expanded, so intervals spanning huge sequence ranges are cheap.

// MysqlBackfillPlan is the normalized planning result for MySQL GTIDs.
// Identity is (SID, tag): intervals of different tags under the same SID
// are never merged.
type MysqlBackfillPlan struct {
	// Pending are the intervals that still need to be collected and can be
	// fetched from the source: (executed - local) - purged.
	Pending MysqlGTIDSet
	// Purged are the intervals that still need to be collected but have
	// already been purged on the source: (executed - local) ∩ purged.
	// These are unrecoverable gaps from this source.
	Purged MysqlGTIDSet
	// Missing are the intervals present locally but absent from the
	// source's executed set: local - executed. The source cannot provide
	// them (local-only or diverged transactions).
	Missing MysqlGTIDSet
}

// PlanMysqlBackfill computes a MySQL backfill plan from the locally
// completed GTID set, the source's executed GTID set and the source's
// purged GTID set. Any of them may be nil, meaning an empty set.
//
// If scope is not nil, all three resulting sets are intersected with it,
// allowing the caller to restrict planning to a subset of SIDs, tags or
// sequence ranges.
func PlanMysqlBackfill(local, executed, purged, scope *MysqlGTIDSet) *MysqlBackfillPlan {
	needed := mysqlSetSubtract(executed, local)

	plan := &MysqlBackfillPlan{
		Pending: mysqlSetSubtractSet(needed, purged),
		Purged:  mysqlSetIntersectSet(needed, purged),
		Missing: mysqlSetSubtract(local, executed),
	}

	if scope != nil {
		plan.Pending = mysqlSetIntersectSet(plan.Pending, scope)
		plan.Purged = mysqlSetIntersectSet(plan.Purged, scope)
		plan.Missing = mysqlSetIntersectSet(plan.Missing, scope)
	}

	return plan
}

// NeedFetch reports whether anything can still be fetched from the source.
// A caller can use it to decide whether connecting to the source is
// worthwhile at all.
func (p *MysqlBackfillPlan) NeedFetch() bool {
	return !p.Pending.IsEmpty()
}

// Complete reports whether nothing is pending, purged or missing.
func (p *MysqlBackfillPlan) Complete() bool {
	return p.Pending.IsEmpty() && p.Purged.IsEmpty() && p.Missing.IsEmpty()
}

func (p *MysqlBackfillPlan) String() string {
	return fmt.Sprintf("pending=%s purged=%s missing=%s",
		p.Pending.String(), p.Purged.String(), p.Missing.String())
}

// normalizedMysqlSet returns a normalized deep copy of s, so planning never
// mutates the caller's sets and always works on sorted, merged intervals.
func normalizedMysqlSet(s *MysqlGTIDSet) MysqlGTIDSet {
	out := NewMysqlGTIDSet()
	if s == nil {
		return out
	}
	for sid, tags := range *s {
		out[sid] = make(map[Tag]IntervalSlice, len(tags))
		for tag, intervals := range tags {
			out[sid][tag] = slices.Clone(intervals).Normalize()
		}
	}
	return out
}

// mysqlSetSubtract returns a - b per (SID, tag) identity.
func mysqlSetSubtract(a, b *MysqlGTIDSet) MysqlGTIDSet {
	return mysqlSetSubtractSet(normalizedMysqlSet(a), b)
}

func mysqlSetSubtractSet(a MysqlGTIDSet, b *MysqlGTIDSet) MysqlGTIDSet {
	out := NewMysqlGTIDSet()
	bn := normalizedMysqlSet(b)
	for sid, tags := range a {
		for tag, intervals := range tags {
			var sub IntervalSlice
			if bTags, ok := bn[sid]; ok {
				sub = intervals.Subtract(bTags[tag])
			} else {
				sub = slices.Clone(intervals)
			}
			if len(sub) > 0 {
				if _, ok := out[sid]; !ok {
					out[sid] = make(map[Tag]IntervalSlice, 1)
				}
				out[sid][tag] = sub
			}
		}
	}
	return out
}

// mysqlSetIntersectSet returns a ∩ b per (SID, tag) identity.
func mysqlSetIntersectSet(a MysqlGTIDSet, b *MysqlGTIDSet) MysqlGTIDSet {
	out := NewMysqlGTIDSet()
	if b == nil {
		return out
	}
	bn := normalizedMysqlSet(b)
	for sid, tags := range a {
		bTags, ok := bn[sid]
		if !ok {
			continue
		}
		for tag, intervals := range tags {
			inter := intervals.Intersect(bTags[tag])
			if len(inter) > 0 {
				if _, ok := out[sid]; !ok {
					out[sid] = make(map[Tag]IntervalSlice, 1)
				}
				out[sid][tag] = inter
			}
		}
	}
	return out
}

// MariadbSeqRange is a half-open sequence range [Start, Stop) of MariaDB
// sequence numbers within one domain. Because a GTID position watermark N
// means "all sequences up to and including N", the range of sequences
// between watermarks L (excluded) and H (included) is [L+1, H+1).
type MariadbSeqRange struct {
	Start uint64
	Stop  uint64
}

func (r MariadbSeqRange) String() string {
	if r.Stop == r.Start+1 {
		return fmt.Sprintf("%d", r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.Stop-1)
}

// MariadbDomainPlan is the planning result for one MariaDB domain.
//
// A MariaDB GTID position is only a high watermark per domain, not a real
// set of intervals. Ranges that cannot be proven from the watermarks are
// reported in Unknown ("不足以判定") instead of being guessed.
type MariadbDomainPlan struct {
	DomainID uint32
	// Pending is the fetchable range of sequences
	// (max(local, purged), min(executed, scope)]. Nil when there is
	// nothing to fetch.
	Pending *MariadbSeqRange
	// Purged is the needed range that has already been purged on the
	// source: (local, min(purged, executed)]. Nil when there is no gap.
	Purged *MariadbSeqRange
	// Unknown is the range the source cannot prove or disprove from its
	// high watermark, e.g. local sequences beyond the source's executed
	// watermark. Nil when everything is decidable.
	Unknown *MariadbSeqRange
	// Reason is a human-readable explanation of the conclusion.
	Reason string
}

func (p MariadbDomainPlan) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "domain %d:", p.DomainID)
	if p.Pending != nil {
		fmt.Fprintf(&sb, " pending=%s", p.Pending)
	}
	if p.Purged != nil {
		fmt.Fprintf(&sb, " purged=%s", p.Purged)
	}
	if p.Unknown != nil {
		fmt.Fprintf(&sb, " unknown=%s", p.Unknown)
	}
	if p.Pending == nil && p.Purged == nil && p.Unknown == nil {
		sb.WriteString(" up-to-date")
	}
	if p.Reason != "" {
		fmt.Fprintf(&sb, " (%s)", p.Reason)
	}
	return sb.String()
}

// MariadbBackfillPlan is the planning result for MariaDB, one entry per
// involved domain, sorted by domain ID.
type MariadbBackfillPlan struct {
	Domains []MariadbDomainPlan
}

// NeedFetch reports whether any domain has a fetchable pending range.
func (p *MariadbBackfillPlan) NeedFetch() bool {
	for _, d := range p.Domains {
		if d.Pending != nil {
			return true
		}
	}
	return false
}

func (p *MariadbBackfillPlan) String() string {
	parts := make([]string, 0, len(p.Domains))
	for _, d := range p.Domains {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, ", ")
}

// PlanMariadbBackfill computes a MariaDB backfill plan from the local
// position, the source's executed (binlog) position and the source's purged
// position. Any of them may be nil, meaning an empty position.
//
// MariaDB positions are high watermarks per domain, not sets. Therefore:
//   - the fetchable range is (max(local, purged), executed],
//   - the purged gap is (local, min(purged, executed)],
//   - if the local watermark is beyond the source's executed watermark, the
//     range (executed, local] cannot be proven from the source and is
//     returned as Unknown ("不足以判定").
//
// If scope is not nil, only domains present in scope are planned and the
// executed watermark is capped at the scope's sequence for that domain,
// allowing the caller to restrict the requested range.
func PlanMariadbBackfill(local, executed, purged, scope *MariadbGTIDSet) *MariadbBackfillPlan {
	domains := make(map[uint32]struct{})
	for _, s := range []*MariadbGTIDSet{local, executed, purged} {
		if s == nil {
			continue
		}
		for domainID := range s.Sets {
			domains[domainID] = struct{}{}
		}
	}
	if scope != nil {
		maps.DeleteFunc(domains, func(domainID uint32, _ struct{}) bool {
			_, ok := scope.Sets[domainID]
			return !ok
		})
	}

	plan := &MariadbBackfillPlan{
		Domains: make([]MariadbDomainPlan, 0, len(domains)),
	}
	for _, domainID := range slices.Sorted(maps.Keys(domains)) {
		plan.Domains = append(plan.Domains, planMariadbDomain(domainID, local, executed, purged, scope))
	}
	return plan
}

func mariadbSeqOf(s *MariadbGTIDSet, domainID uint32) (serverID uint32, seq uint64, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	gtid, ok := s.Sets[domainID]
	if !ok {
		return 0, 0, false
	}
	return gtid.ServerID, gtid.SequenceNumber, true
}

func planMariadbDomain(domainID uint32, local, executed, purged, scope *MariadbGTIDSet) MariadbDomainPlan {
	localServer, localSeq, localOK := mariadbSeqOf(local, domainID)
	execServer, execSeq, execOK := mariadbSeqOf(executed, domainID)
	_, purgedSeq, _ := mariadbSeqOf(purged, domainID)

	if scope != nil {
		if _, scopeSeq, ok := mariadbSeqOf(scope, domainID); ok && scopeSeq < execSeq {
			execSeq = scopeSeq
		}
	}

	dp := MariadbDomainPlan{DomainID: domainID}

	var reasons []string
	if localOK && execOK && localServer != execServer {
		reasons = append(reasons, fmt.Sprintf(
			"server_id changed from %d to %d; sequence comparison assumes domain continuity",
			localServer, execServer))
	}

	switch {
	case localSeq > execSeq:
		// The local side claims history beyond the source's high watermark;
		// the source cannot prove or disprove it.
		r := MariadbSeqRange{Start: execSeq + 1, Stop: localSeq + 1}
		dp.Unknown = &r
		reasons = append(reasons,
			"local position exceeds source executed high watermark; indeterminate from source")
	case execSeq > localSeq:
		if gapStop := min(purgedSeq, execSeq); gapStop > localSeq {
			r := MariadbSeqRange{Start: localSeq + 1, Stop: gapStop + 1}
			dp.Purged = &r
			reasons = append(reasons, "needed sequences already purged on source")
		}
		if start := max(localSeq, purgedSeq); execSeq > start {
			r := MariadbSeqRange{Start: start + 1, Stop: execSeq + 1}
			dp.Pending = &r
		}
	}

	dp.Reason = strings.Join(reasons, "; ")
	return dp
}
