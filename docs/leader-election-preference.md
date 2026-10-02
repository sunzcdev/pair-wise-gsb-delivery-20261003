# Candidate Priority (Leader Election Preference)

Some Raft servers are better candidates for leader (closer to clients, faster
hardware, a preferred rack/zone), but the cluster must still elect a leader
when those servers are unreachable. This implementation adds an **optional,
bounded candidate preference** on top of the standard Raft election rules.

## Configuration

Two fields on `Config` control the feature; both default to off, so an
unmodified configuration behaves exactly like classic Raft:

- `CandidatePriority int` (default `0`) — this server's local preference.
  Higher is more preferred. `0` means "no preference". The value is sent in
  `RequestVoteRequest` and `RequestPreVoteRequest`.
- `ElectionPreferenceTimeout time.Duration` (default `0`) — how long a voter
  may wait for a reachable, higher-priority candidate before granting a vote.
  `0` disables the preference entirely. When non-zero it is validated to be
  **strictly smaller than `HeartbeatTimeout`**, so the wait can never reach or
  exceed the normal election timeout.

Priority is a *preference hint*, never a quorum bypass: electing a leader
still requires a majority, and every ordinary Raft safety rule is enforced.

## How it works

When preference is enabled and a follower is asked for a vote, it runs the
usual eligibility checks first (configuration membership, suffrage, term, and
log recency). Only an otherwise-eligible request enters the preference logic.

A single **election window** is anchored to the first campaigning activity for
a prospective term and covers both the pre-vote phase and the binding vote
phase (the wait budget is therefore not reset between the phases). During the
window the voter:

1. Records the highest candidate priority observed in the election.
2. Defers its decision until the window closes.
3. If this server itself outranks the campaigning candidate, immediately starts
   its own pre-vote campaign via a coalesced internal signal. This is what lets
   a preferred node that only starts *slightly late* still win the round.

When the bounded window closes the voter grants to candidates at the highest
priority observed and refuses lower-priority ones. Once the window has elapsed
the election proceeds under ordinary rules, so an unreachable preferred server
cannot block availability.

## Safety invariants

The preference layer never weakens Raft safety:

- **Log recency beats preference.** A high-priority candidate whose log is
  behind the voter's (`lastLogTerm`, then `lastLogIndex`) is rejected before
  any preference is considered.
- **Terms are still monotonic.** Votes for older terms are rejected, and a
  higher term still forces a step-down.
- **One vote per server per term.** The deferred path persists a vote through
  the same durable `persistVote` mechanism, re-checks the persisted vote at
  settle time, and an RPC is answered exactly once.
- **Majority is always required.** Preference only orders otherwise-valid
  votes; it cannot make a minority leader.

## Feature interactions

### PreVote

PreVote (enabled by default) is the primary mechanism that makes a
slightly-late preferred server win. Pre-vote is advisory and non-binding, so:

- A server that is itself a candidate never defers or vetoes a competing
  same-round pre-vote; doing so would livelock equal candidates. Pre-vote
  comparisons allow the prospective term (`currentTerm + 1`) when we are
  campaigning.
- The highest priority observed during pre-vote carries into the binding vote
  window, so a candidate cannot win pre-vote and then sneak a real vote past
  an unobserved higher-priority server.
- The pre-vote phase honours the same bounded window; after the total wait
  elapses, pre-votes are granted to the highest-priority candidates seen.

With PreVote disabled the binding-vote path still applies the same bounded
wait; availability remains guaranteed and the window still expires.

### Leadership transfer

An explicit `LeadershipTransfer`/`LeadershipTransferToServer` bypasses
preference completely. The transfer target's `RequestVote` carries
`LeadershipTransfer: true`, which:

- skips the preference window (the vote is answered immediately), and
- allows a vote even though a leader is known.

Transfers already skip pre-vote. Therefore a low-priority, explicitly chosen
target takes over promptly regardless of configured priorities.

### Old / mixed-version servers

`Priority` is an additive field on the two vote requests. With msgpack an old
binary that does not know the field simply decodes it as `0`:

- An old voter (or any server with `ElectionPreferenceTimeout == 0`) ignores
  priority and votes with standard Raft rules.
- An old candidate advertises priority `0`, identical to a non-preferred new
  candidate.

Consequently a rolling upgrade is safe: as soon as enough voters honour
preference the preferred node tends to win; while old voters remain, elections
still terminate and may simply be unprioritised. Preference is never required
for progress and never partitions the quorum.
