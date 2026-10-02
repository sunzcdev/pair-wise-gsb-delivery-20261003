# Candidate Priority

Some servers are better suited to be leaders (faster machines, better
network placement, closer to clients). Candidate priorities let an operator
express that preference so that, within the same election round, a reachable
higher-priority server is more likely to win — while an unreachable
preferred server can never block the cluster from electing someone else.

Priority is a *preference only*. It never overrides the safety rules of the
protocol: term checks, one vote per term, log freshness, and quorum
requirements all take precedence. A high-priority server still needs a
majority of votes to lead.

## Configuration

* `Server.Priority` (in the cluster `Configuration`) is a `uint32`; higher
  means more preferred. The zero value means "no preference" and is the
  default, so clusters that never set priorities behave exactly as before.
  Priorities are replicated to every server as part of the configuration, so
  all voters agree on each server's priority.
* `Config.MaxCandidatePriorityWait` bounds how long priorities may delay an
  election. The zero value defaults to `ElectionTimeout`.

## Mechanisms

Two cooperating mechanisms implement the preference, both bounded by
`MaxCandidatePriorityWait`:

1. **Delayed candidacy.** When a server becomes a candidate and a
   higher-priority voter exists in the configuration, it waits before
   soliciting votes. The delay scales linearly with the priority gap
   (`MaxCandidatePriorityWait * (maxPriority - myPriority) / maxPriority`)
   and is additionally capped by the candidate-state election timeout, so a
   low-priority server always gets to campaign within one election timeout
   even if `MaxCandidatePriorityWait` is set higher.
2. **Vote withholding.** A voter that is about to grant a vote (or pre-vote)
   to a candidate whose priority is below the maximum in the configuration
   temporarily withholds it, giving a higher-priority server a chance to
   campaign. Withholding starts a per-server window of at most
   `MaxCandidatePriorityWait`; once the window expires, votes are granted
   normally until the window is reset by a granted vote or by contact from a
   leader. The wait is therefore always bounded, and a preferred server that
   is partitioned away from the majority only slows an election down by at
   most this duration.

## Interaction with other features

* **Safety rules.** All existing checks in `requestVote`/`requestPreVote`
  (term, already-voted, log freshness) run before the priority check.
  Priority can only ever turn a would-be grant into a temporary denial,
  never a denial into a grant. A high-priority candidate with a stale log is
  still rejected, and a recorded vote is never stolen by a higher-priority
  candidate in the same term.
* **PreVote.** Pre-vote requests go through the same withholding logic as
  real votes, so a partitioned low-priority server cannot disrupt the
  cluster, and a partitioned high-priority server does not inflate terms
  while the majority waits out the bounded window. The candidacy delay
  applies before the pre-vote phase starts.
* **Leadership transfer.** A candidacy triggered by `TimeoutNow` (an
  explicit leadership transfer) is never delayed, and vote requests carrying
  the `LeadershipTransfer` flag are never withheld: explicit operator intent
  beats the preference. When `LeadershipTransfer()` is called without a
  target, the leader picks the highest-priority up-to-date follower as the
  transfer target (ties broken by log position, as before).
* **Old servers.** Servers running older code ignore the unknown `Priority`
  field when decoding configurations (and re-encode configurations without
  it). They never delay their candidacy and never withhold votes — they
  vote by the classic rules — and new servers treat them as priority 0.
  Elections in a mixed cluster remain safe because priority only affects
  timing, never the safety rules. Note that a configuration change committed
  by an old-code leader drops priority information, so all servers should be
  upgraded before priorities are relied upon.
