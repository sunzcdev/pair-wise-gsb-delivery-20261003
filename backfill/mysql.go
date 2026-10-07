package backfill

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"
)

// mysqlIdentity identifies a MySQL transaction stream. Intervals belonging to
// different tags of the same SID are never merged: MySQL treats each
// (SID, tag) pair as an independent sequence space.
type mysqlIdentity struct {
	sid uuid.UUID
	tag mysql.Tag
}

func (id mysqlIdentity) String() string {
	if tag := id.tag.String(); tag != "" {
		return id.sid.String() + ":" + tag
	}
	return id.sid.String()
}

// MysqlPlan is the offline backfill plan for a MySQL (or MySQL-tagged-GTID)
// source. Every gap field is a normalized GTID set keyed by (SID, tag);
// no transaction number is ever materialized.
type MysqlPlan struct {
	// Requested is the scope the caller asked to converge to, after
	// normalization. It defaults to local union source-executed.
	Requested *mysql.MysqlGTIDSet
	// Fetchable holds transactions the source has executed, has not purged,
	// and the replica is missing. These can be requested from the source.
	Fetchable *mysql.MysqlGTIDSet
	// SourceMissing holds requested transactions the source has never
	// executed. They can only appear when the caller narrows the scope.
	SourceMissing *mysql.MysqlGTIDSet
	// PurgedGap holds requested transactions that are missing locally but
	// have already been purged on the source, so the source cannot replay
	// them.
	PurgedGap *mysql.MysqlGTIDSet
	// LocalAhead holds requested transactions the replica already owns but
	// the source has never executed (divergent history). It is explanatory
	// and never asked from this source.
	LocalAhead *mysql.MysqlGTIDSet
	// Action is the offline connection recommendation.
	Action ConnectAction
	// Reasons explains the classification, one human-readable line per
	// (SID, tag) stream plus a summary line.
	Reasons []string
}

// PlanMySQL classifies the catch-up space for a MySQL source.
//
// local is the GTID set the replica has completed, executed is the source's
// gtid_executed and purged is the source's gtid_purged. All three must use
// the same (SID, tag) keying; purged must be a subset of executed.
//
// scope optionally narrows the transaction range the caller is interested in.
// When scope is nil the effective scope is local union executed, which means
// every requested transaction is known somewhere and SourceMissing is empty.
// Pass an explicit scope (for example a target topology set) to detect
// requested transactions the source never executed.
//
// The function only performs interval-set arithmetic; it never contacts the
// source and never iterates individual transaction numbers.
func PlanMySQL(local, executed, purged, scope *mysql.MysqlGTIDSet) (*MysqlPlan, error) {
	local = nonNilSet(local)
	executed = nonNilSet(executed)
	purged = nonNilSet(purged)

	if !executed.Contain(purged) {
		return nil, fmt.Errorf("backfill: source gtid_purged (%s) is not a subset of gtid_executed (%s)", purged, executed)
	}

	requested := unionSets(local, executed)
	if scope != nil {
		requested = intersectSets(requested, scope)
	}

	fetchableSet := mysql.NewMysqlGTIDSet()
	sourceMissingSet := mysql.NewMysqlGTIDSet()
	purgedSet := mysql.NewMysqlGTIDSet()
	localAheadSet := mysql.NewMysqlGTIDSet()
	plan := &MysqlPlan{
		Requested:     requested,
		Fetchable:     &fetchableSet,
		SourceMissing: &sourceMissingSet,
		PurgedGap:     &purgedSet,
		LocalAhead:    &localAheadSet,
		Action:        ActionSkip,
	}

	identities := collectIdentities(local, executed, purged, requested)
	for _, id := range identities {
		want := (*requested)[id.sid][id.tag]
		have := (*local)[id.sid][id.tag]
		done := (*executed)[id.sid][id.tag]
		gone := (*purged)[id.sid][id.tag]

		missingHere := subtractIntervals(want, have)
		onSource := intersectIntervals(missingHere, done)

		fetchable := subtractIntervals(onSource, gone)
		sourceMissing := subtractIntervals(missingHere, onSource)
		purgedGap := subtractIntervals(onSource, fetchable)
		// LocalAhead only covers transactions the source has never executed.
		// Transactions present on both sides are already-complete history,
		// not divergent.
		localAhead := intersectIntervals(subtractIntervals(have, done), want)

		putIntervals(plan.Fetchable, id, fetchable)
		putIntervals(plan.SourceMissing, id, sourceMissing)
		putIntervals(plan.PurgedGap, id, purgedGap)
		putIntervals(plan.LocalAhead, id, localAhead)

		var clauses []string
		if len(fetchable) > 0 {
			clauses = append(clauses, intervalSummary(fetchable)+" fetchable")
		}
		if len(purgedGap) > 0 {
			clauses = append(clauses, intervalSummary(purgedGap)+" purged on source")
		}
		if len(sourceMissing) > 0 {
			clauses = append(clauses, intervalSummary(sourceMissing)+" never executed on source")
		}
		if len(localAhead) > 0 {
			clauses = append(clauses, intervalSummary(localAhead)+" held locally but not on source")
		}
		if len(clauses) > 0 {
			plan.Reasons = append(plan.Reasons, id.String()+": "+strings.Join(clauses, ", "))
		}
	}

	if !plan.Fetchable.IsEmpty() {
		plan.Action = ActionConnect
	}

	summary := fmt.Sprintf(
		"mysql: requested %d stream(s); action=%s; fetchable=%t, source_missing=%t, purged_gap=%t, local_ahead=%t",
		len(identities), plan.Action,
		!plan.Fetchable.IsEmpty(), !plan.SourceMissing.IsEmpty(),
		!plan.PurgedGap.IsEmpty(), !plan.LocalAhead.IsEmpty())
	plan.Reasons = append([]string{summary}, plan.Reasons...)

	return plan, nil
}

// PlanMySQLFromStrings is the convenience form of [PlanMySQL] for callers
// that hold the three sets in their MySQL GTID-set string representation.
// scope may be empty to request local union source-executed.
func PlanMySQLFromStrings(localSet, executedSet, purgedSet, scopeSet string) (*MysqlPlan, error) {
	local, err := parseMysql(localSet)
	if err != nil {
		return nil, err
	}
	executed, err := parseMysql(executedSet)
	if err != nil {
		return nil, err
	}
	purged, err := parseMysql(purgedSet)
	if err != nil {
		return nil, err
	}
	var scope *mysql.MysqlGTIDSet
	if strings.TrimSpace(scopeSet) != "" {
		scope, err = parseMysql(scopeSet)
		if err != nil {
			return nil, err
		}
	}
	return PlanMySQL(local, executed, purged, scope)
}

func parseMysql(s string) (*mysql.MysqlGTIDSet, error) {
	parsed, err := mysql.ParseMysqlGTIDSet(s)
	if err != nil {
		return nil, fmt.Errorf("backfill: parse mysql gtid set %q: %w", s, err)
	}
	set, ok := parsed.(*mysql.MysqlGTIDSet)
	if !ok {
		// ParseMysqlGTIDSet always returns *MysqlGTIDSet.
		return nil, fmt.Errorf("backfill: unexpected mysql gtid set type %T", parsed)
	}
	return set, nil
}

func nonNilSet(s *mysql.MysqlGTIDSet) *mysql.MysqlGTIDSet {
	if s == nil {
		v := mysql.NewMysqlGTIDSet()
		return &v
	}
	return s
}

// collectIdentities returns every (SID, tag) present in any input, sorted by
// UUID then tag for deterministic output.
func collectIdentities(sets ...*mysql.MysqlGTIDSet) []mysqlIdentity {
	seen := make(map[mysqlIdentity]struct{})
	for _, set := range sets {
		for sid, tags := range *set {
			for tag := range tags {
				seen[mysqlIdentity{sid, tag}] = struct{}{}
			}
		}
	}
	out := make([]mysqlIdentity, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].sid != out[j].sid {
			return out[i].sid.String() < out[j].sid.String()
		}
		return out[i].tag.String() < out[j].tag.String()
	})
	return out
}

func putIntervals(set *mysql.MysqlGTIDSet, id mysqlIdentity, intervals mysql.IntervalSlice) {
	if len(intervals) == 0 {
		return
	}
	if _, ok := (*set)[id.sid]; !ok {
		(*set)[id.sid] = make(map[mysql.Tag]mysql.IntervalSlice, 1)
	}
	(*set)[id.sid][id.tag] = intervals
}

func intervalSummary(intervals mysql.IntervalSlice) string {
	if len(intervals) == 0 {
		return "nothing"
	}
	parts := make([]string, len(intervals))
	for i, iv := range intervals {
		parts[i] = iv.String()
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// The helpers below implement interval-set algebra on [start, stop) half-open
// intervals in linear time, without ever expanding sequence numbers.

func subtractIntervals(a, b mysql.IntervalSlice) mysql.IntervalSlice {
	if len(a) == 0 {
		return nil
	}
	if len(b) == 0 {
		return append(mysql.IntervalSlice(nil), a...)
	}
	var out mysql.IntervalSlice
	j := 0
	for _, ai := range a {
		cur := ai
		for j < len(b) && b[j].Stop <= cur.Start {
			j++
		}
		k := j
		for k < len(b) && b[k].Start < cur.Stop {
			if b[k].Start > cur.Start {
				out = append(out, mysql.Interval{Start: cur.Start, Stop: b[k].Start})
			}
			if b[k].Stop > cur.Start {
				cur.Start = b[k].Stop
			}
			if cur.Start >= cur.Stop {
				break
			}
			k++
		}
		if cur.Start < cur.Stop {
			out = append(out, cur)
		}
	}
	return out
}

func intersectIntervals(a, b mysql.IntervalSlice) mysql.IntervalSlice {
	var out mysql.IntervalSlice
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		start := max(a[i].Start, b[j].Start)
		stop := min(a[i].Stop, b[j].Stop)
		if start < stop {
			out = append(out, mysql.Interval{Start: start, Stop: stop})
		}
		if a[i].Stop < b[j].Stop {
			i++
		} else {
			j++
		}
	}
	return out
}

func unionIntervals(a, b mysql.IntervalSlice) mysql.IntervalSlice {
	out := make(mysql.IntervalSlice, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out.Normalize()
}

func foldSets(a, b *mysql.MysqlGTIDSet, combine func(mysql.IntervalSlice, mysql.IntervalSlice) mysql.IntervalSlice) *mysql.MysqlGTIDSet {
	out := mysql.NewMysqlGTIDSet()
	for _, id := range collectIdentities(a, b) {
		var ai, bi mysql.IntervalSlice
		if m, ok := (*a)[id.sid]; ok {
			ai = m[id.tag]
		}
		if m, ok := (*b)[id.sid]; ok {
			bi = m[id.tag]
		}
		putIntervals(&out, id, combine(ai, bi))
	}
	return &out
}

func unionSets(a, b *mysql.MysqlGTIDSet) *mysql.MysqlGTIDSet {
	return foldSets(a, b, unionIntervals)
}

func intersectSets(a, b *mysql.MysqlGTIDSet) *mysql.MysqlGTIDSet {
	return foldSets(a, b, intersectIntervals)
}
