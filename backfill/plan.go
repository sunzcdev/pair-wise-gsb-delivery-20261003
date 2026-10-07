// Package backfill plans post-disaster transaction catch-up ("backfill")
// against an upstream data source.
//
// A caller feeds the planner three snapshots it already collected locally:
//
//   - the GTID set the replica has completed,
//   - the GTID set the source has executed,
//   - the GTID set the source has already purged.
//
// The planner works purely offline: it classifies the requested transaction
// range before the caller decides whether opening a connection to the source
// is worthwhile. All interval math works on whole intervals; individual
// transaction numbers are never expanded.
//
// MySQL and MariaDB GTIDs have deliberately different semantics, so the
// package exposes two separate planners with two separate input models:
//
//   - [PlanMySQL] reasons over MySQL GTID sets, where identity is the
//     (server UUID, tag) pair and transactions form explicit interval sets.
//   - [PlanMariaDB] reasons over MariaDB GTID high-water marks, where history
//     can only be proven under the domain gaplessness invariant and some
//     ranges must be reported as impossible to judge offline.
package backfill

import "fmt"

// ConnectAction is the planner's offline recommendation for what the caller
// should do next.
type ConnectAction int

const (
	// ActionSkip means nothing provable can be gained by connecting: within the
	// requested scope every missing transaction is either purged on the source,
	// never executed on the source, or already present locally.
	ActionSkip ConnectAction = iota
	// ActionConnect means the source demonstrably still holds transactions that
	// the replica is missing. The caller may connect and request them.
	ActionConnect
	// ActionInspect means the request cannot be classified from high-water marks
	// alone (MariaDB-specific). The caller must connect to inspect the source's
	// retention state before deciding what is fetchable.
	ActionInspect
)

// String renders the action for logs and explanations.
func (a ConnectAction) String() string {
	switch a {
	case ActionSkip:
		return "skip"
	case ActionConnect:
		return "connect"
	case ActionInspect:
		return "inspect-source"
	default:
		return fmt.Sprintf("ConnectAction(%d)", int(a))
	}
}
