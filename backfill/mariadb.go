package backfill

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// MariaDB GTID semantics differ fundamentally from MySQL GTID sets:
//
//   - A GTID is domain-server-sequence. Sequences are allocated per
//     replication domain, not per server.
//   - The executed/state variables (gtid_binlog_state, gtid_binlog_pos) hold
//     one GTID per (domain, server) pair and therefore only establish
//     high-water marks: "server X executed sequence N in domain D".
//   - History is gapless inside a domain only under MariaDB's single-primary
//     ordering invariant (see
//     https://mariadb.com/kb/en/gtid/). Whether the interval between the
//     local mark and the source mark can still be fetched additionally
//     depends on the source's binlog retention, which the executed state
//     alone cannot prove.
//
// The planner therefore never pretends MariaDB has MySQL-style interval sets.
// It returns proven prefixes, and every range the high-water marks cannot
// prove is classified Indeterminate.

// MariaDBState is a snapshot of MariaDB binlog state, keyed by
// (domain id, server id) -> highest sequence number. Unlike
// [mysql.MariadbGTIDSet], it deliberately preserves every contributing
// server: collapsing the entries into one position per domain would destroy
// exactly the failover attribution information the planner needs.
type MariaDBState map[MariaDBNode]uint64

// MariaDBNode identifies one writer of a MariaDB replication domain.
type MariaDBNode struct {
	DomainID uint32
	ServerID uint32
}

// ParseMariaDBState parses a comma-separated "domain-server-sequence" list,
// for example the value of gtid_binlog_state or the Gtid_list of the oldest
// retained binlog file. Duplicate (domain, server) entries keep the highest
// sequence number.
func ParseMariaDBState(s string) (MariaDBState, error) {
	state := MariaDBState{}
	if strings.TrimSpace(s) == "" {
		return state, nil
	}
	for _, part := range strings.Split(s, ",") {
		gtid, err := mysql.ParseMariadbGTID(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("backfill: parse mariadb state %q: %w", part, err)
		}
		node := MariaDBNode{DomainID: gtid.DomainID, ServerID: gtid.ServerID}
		if gtid.SequenceNumber > state[node] {
			state[node] = gtid.SequenceNumber
		}
	}
	return state, nil
}

// MariaDBScope narrows a backfill request to the given highest desired
// sequence number per domain. Domains absent from the scope default to the
// union of the local and source high-water marks.
type MariaDBScope map[uint32]uint64

// SequenceRange is an inclusive range of MariaDB domain sequence numbers.
// Ranges describe prefixes and tails; individual sequences are never
// enumerated.
type SequenceRange struct {
	From uint64
	To   uint64
}

func (r SequenceRange) String() string {
	if r.From == r.To {
		return fmt.Sprintf("%d", r.From)
	}
	return fmt.Sprintf("%d-%d", r.From, r.To)
}

// MariaDBSourcePoint records one source writer's executed high-water mark for
// a domain, keeping the server attribution visible in the explanation.
type MariaDBSourcePoint struct {
	ServerID uint32
	Sequence uint64
}

// MariaDBDomainPlan is the classification of one replication domain.
type MariaDBDomainPlan struct {
	DomainID uint32
	// Target is the highest sequence number the caller wants to converge to.
	Target uint64
	// LocalHigh is the highest completed sequence the replica holds for the
	// domain.
	LocalHigh uint64
	// ExecutedHigh is the highest executed sequence on the source for the
	// domain. It is 0 when the source has never executed the domain.
	ExecutedHigh uint64
	// Executed is the source's executed high-water mark per contributing
	// server, sorted by sequence number. ServerCount > 1 means the domain
	// history crosses a failover boundary.
	Executed []MariaDBSourcePoint
	// PurgeHigh is the domain-wide purge frontier: sequences at or below it
	// are proven purged. It is 0 when nothing is known to be purged.
	PurgeHigh uint64
	// PurgedKnown reports whether source retention information was provided.
	// When false, the range between the local and executed high-water marks
	// cannot be proven fetchable and is reported Indeterminate.
	PurgedKnown bool
	// Fetchable is the proven retained tail the source can still serve.
	Fetchable []SequenceRange
	// SourceMissing is the requested tail the source has never executed.
	SourceMissing []SequenceRange
	// Purged is the requested prefix proven to have been removed on the
	// source.
	Purged []SequenceRange
	// Indeterminate is the historical range high-water marks cannot prove.
	Indeterminate []SequenceRange
	// LocalAhead is the completed prefix beyond what the source has
	// executed (the replica is ahead or the domain is foreign to the
	// source).
	LocalAhead []SequenceRange
	// Reason is the human-readable explanation for this domain.
	Reason string
}

// MariaDBPlan is the offline backfill plan for a MariaDB source.
type MariaDBPlan struct {
	Domains []MariaDBDomainPlan
	// Action is the offline connection recommendation.
	Action ConnectAction
	// Reasons explains the overall decision, one line per domain plus a
	// summary line.
	Reasons []string
}

// PlanMariaDB classifies the catch-up space for a MariaDB source.
//
// local is the replica's completed state (one or more
// domain-server-sequence entries), executed is the source's gtid_binlog_state
// and purged is the state derived from the Gtid_list of the source's oldest
// retained binlog file. Pass purgedKnown=false and nil purged when retention
// information is unavailable; every unproven historical range is then
// reported as indeterminate instead of being guessed.
//
// scope optionally limits the highest desired sequence per domain. The
// planner only reasons with high-water marks and inclusive sequence ranges;
// it never iterates individual sequence numbers.
func PlanMariaDB(local, executed, purged MariaDBState, purgedKnown bool, scope MariaDBScope) (*MariaDBPlan, error) {
	localByDomain := foldMariaDBState(local)
	executedByDomain := groupMariaDBState(executed)
	purgedByDomain := foldMariaDBState(purged)

	if purgedKnown {
		for domainID, purgeHigh := range purgedByDomain {
			points := executedByDomain[domainID]
			if len(points) == 0 {
				return nil, fmt.Errorf("backfill: domain %d has a purged frontier but no executed entry", domainID)
			}
			if purgeHigh > points[len(points)-1].Sequence {
				return nil, fmt.Errorf("backfill: domain %d purged frontier %d exceeds source executed high-water mark %d",
					domainID, purgeHigh, points[len(points)-1].Sequence)
			}
		}
	}

	domains := collectMariaDBDomains(localByDomain, executedByDomain, purgedByDomain, scope)
	plan := &MariaDBPlan{Domains: make([]MariaDBDomainPlan, 0, len(domains))}

	indeterminateAny := false
	fetchableAny := false

	for _, domainID := range domains {
		entry := MariaDBDomainPlan{
			DomainID:    domainID,
			LocalHigh:   localByDomain[domainID],
			Executed:    executedByDomain[domainID],
			PurgedKnown: purgedKnown,
		}

		h := uint64(0)
		if len(entry.Executed) > 0 {
			h = entry.Executed[len(entry.Executed)-1].Sequence
		}
		entry.ExecutedHigh = h

		if target, ok := scope[domainID]; ok {
			entry.Target = target
		} else {
			entry.Target = max(entry.LocalHigh, h)
		}

		if purgedKnown {
			entry.PurgeHigh = purgedByDomain[domainID]
		}

		if h == 0 {
			// The source has never executed this domain. Nothing can be
			// fetched; the whole requested tail is missing on the source.
			entry.SourceMissing = inclusiveRange(entry.LocalHigh+1, entry.Target)
			entry.LocalAhead = inclusiveRange(1, entry.LocalHigh)
			entry.Reason = fmt.Sprintf(
				"domain %d: source has never executed this domain; %s missing on source, local completed through %d",
				domainID, summarizeRanges(entry.SourceMissing), entry.LocalHigh)
		} else {
			p := entry.PurgeHigh
			l := entry.LocalHigh

			if purgedKnown {
				entry.Purged = inclusiveRange(l+1, min(p, h))
				entry.Fetchable = inclusiveRange(max(l, p)+1, h)
				entry.Indeterminate = nil
			} else {
				entry.Indeterminate = inclusiveRange(l+1, h)
			}
			entry.SourceMissing = inclusiveRange(h+1, entry.Target)
			entry.LocalAhead = inclusiveRange(h+1, l)

			clauses := []string{
				fmt.Sprintf("local through %d, source executed through %d (%d writer(s))",
					l, h, len(entry.Executed)),
			}
			if purgedKnown {
				clauses = append(clauses,
					fmt.Sprintf("purge frontier %d", p),
					summarizeRanges(entry.Fetchable)+" provably fetchable",
					summarizeRanges(entry.Purged)+" purged",
				)
			} else {
				clauses = append(clauses,
					"retention unknown: "+summarizeRanges(entry.Indeterminate)+
						" cannot be proven fetchable from high-water marks",
				)
			}
			if len(entry.SourceMissing) > 0 {
				clauses = append(clauses, summarizeRanges(entry.SourceMissing)+" beyond source high-water mark")
			}
			entry.Reason = "domain " + fmt.Sprintf("%d", domainID) + ": " + strings.Join(clauses, "; ")
		}

		if len(entry.Fetchable) > 0 {
			fetchableAny = true
		}
		if len(entry.Indeterminate) > 0 {
			indeterminateAny = true
		}

		plan.Domains = append(plan.Domains, entry)
	}

	switch {
	case fetchableAny:
		plan.Action = ActionConnect
	case indeterminateAny:
		plan.Action = ActionInspect
	default:
		plan.Action = ActionSkip
	}

	for i := range plan.Domains {
		plan.Reasons = append(plan.Reasons, plan.Domains[i].Reason)
	}
	summary := fmt.Sprintf("mariadb: %d domain(s); action=%s", len(plan.Domains), plan.Action)
	plan.Reasons = append([]string{summary}, plan.Reasons...)

	return plan, nil
}

// PlanMariaDBFromStrings is the convenience form of [PlanMariaDB].
//
// localSet and executedSet accept comma-separated domain-server-sequence
// lists. purgedSet carries the same format. Set purgedKnown to false to
// declare that retention information is unavailable (purgedSet is ignored);
// set it to true with an empty purgedSet to declare that nothing has been
// purged. scope may be nil to request the local/executed high-water union.
func PlanMariaDBFromStrings(localSet, executedSet, purgedSet string, purgedKnown bool, scope MariaDBScope) (*MariaDBPlan, error) {
	local, err := ParseMariaDBState(localSet)
	if err != nil {
		return nil, err
	}
	executed, err := ParseMariaDBState(executedSet)
	if err != nil {
		return nil, err
	}
	var purged MariaDBState
	if purgedKnown {
		purged, err = ParseMariaDBState(purgedSet)
		if err != nil {
			return nil, err
		}
	}
	return PlanMariaDB(local, executed, purged, purgedKnown, scope)
}

// foldMariaDBState reduces a state snapshot to one high-water mark per
// domain, ignoring which server produced it. Used for local (slave) position
// and purge-frontier inputs.
func foldMariaDBState(state MariaDBState) map[uint32]uint64 {
	out := make(map[uint32]uint64)
	for node, sequence := range state {
		if sequence > out[node.DomainID] {
			out[node.DomainID] = sequence
		}
	}
	return out
}

// groupMariaDBState keeps every source writer per domain, sorted by sequence.
func groupMariaDBState(state MariaDBState) map[uint32][]MariaDBSourcePoint {
	out := make(map[uint32][]MariaDBSourcePoint)
	for node, sequence := range state {
		out[node.DomainID] = append(out[node.DomainID], MariaDBSourcePoint{
			ServerID: node.ServerID,
			Sequence: sequence,
		})
	}
	for domainID := range out {
		sort.Slice(out[domainID], func(i, j int) bool {
			if out[domainID][i].Sequence != out[domainID][j].Sequence {
				return out[domainID][i].Sequence < out[domainID][j].Sequence
			}
			return out[domainID][i].ServerID < out[domainID][j].ServerID
		})
	}
	return out
}

func collectMariaDBDomains(
	local map[uint32]uint64,
	executed map[uint32][]MariaDBSourcePoint,
	purged map[uint32]uint64,
	scope MariaDBScope,
) []uint32 {
	seen := make(map[uint32]struct{})
	for domainID := range local {
		seen[domainID] = struct{}{}
	}
	for domainID := range executed {
		seen[domainID] = struct{}{}
	}
	for domainID := range purged {
		seen[domainID] = struct{}{}
	}
	for domainID := range scope {
		seen[domainID] = struct{}{}
	}
	out := make([]uint32, 0, len(seen))
	for domainID := range seen {
		out = append(out, domainID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func inclusiveRange(from, to uint64) []SequenceRange {
	if from == 0 || from > to {
		return nil
	}
	return []SequenceRange{{From: from, To: to}}
}

func summarizeRanges(ranges []SequenceRange) string {
	if len(ranges) == 0 {
		return "nothing"
	}
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.String()
	}
	return "[" + strings.Join(parts, ",") + "]"
}
