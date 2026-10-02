// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import "time"

// logAtLeastAsUpToDate reports whether a candidate's log tail is at least as
// up-to-date as ours, using the standard Raft comparison (last term, then last
// index). Preference never overrides this: a stale high-priority candidate is
// still rejected.
func (r *Raft) logAtLeastAsUpToDate(lastLogIndex, lastLogTerm uint64) bool {
	lastIdx, lastTerm := r.getLastEntry()
	if lastTerm > lastLogTerm {
		return false
	}
	if lastTerm == lastLogTerm && lastIdx > lastLogIndex {
		return false
	}
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// pendingVote records a vote or pre-vote request whose decision is deferred
// until the bounded preference window closes. decide re-runs every ordinary
// eligibility check and either grants or rejects; it runs on the main
// goroutine and is responsible for sending the RPC response exactly once.
type pendingVote struct {
	priority int
	decide   func(grant bool)
}

// electionWindow is the single bounded wait covering one election: it opens at
// the first campaigning activity for a prospective term and spans both the
// advisory pre-vote phase and the binding vote phase. Anchoring one window to
// the election means the wait budget is never reset between the phases.
type electionWindow struct {
	term       uint64
	deadline   time.Time
	timer      *time.Timer
	generation uint64

	// maxObservedPriority is the highest priority seen during the election
	// (both phases combined), seeded with this server's own priority.
	maxObservedPriority int

	prevotePending []pendingVote
	votePending    []pendingVote

	settled bool
}

// preferenceDecision tells the caller how to respond.
type preferenceDecision int

const (
	// preferenceAnswerNow: preference is inert; run the ordinary grant path.
	preferenceAnswerNow preferenceDecision = iota
	// preferenceRejected: decide(false) was invoked inline.
	preferenceRejected
	// preferenceDeferred: decide runs later when the window closes.
	preferenceDeferred
)

func (r *Raft) preferenceEnabled() bool {
	return r.electionPreferenceTimeout > 0
}

// windowFor returns the window covering the election for term, creating one
// anchored at the first activity when needed. Only called on the main
// goroutine.
func (r *Raft) windowFor(term uint64) *electionWindow {
	w := &r.preferenceWindow
	if w.term == term {
		return w
	}

	r.closePreferenceWindow(false)

	r.preferenceGeneration++
	gen := r.preferenceGeneration
	now := time.Now()
	r.preferenceWindow = electionWindow{
		term:                term,
		deadline:            now.Add(r.electionPreferenceTimeout),
		generation:          gen,
		maxObservedPriority: r.candidatePriority,
	}
	w = &r.preferenceWindow
	w.timer = time.AfterFunc(time.Until(w.deadline), func() {
		select {
		case r.preferenceSettleCh <- settleRequest{term: term, generation: gen}:
		case <-r.shutdownCh:
		}
	})
	return w
}

// closePreferenceWindow releases deferred decisions and stops the timer. With
// settle true the requests resolve by priority; otherwise they are rejected
// (state resets).
func (r *Raft) closePreferenceWindow(settle bool) {
	w := &r.preferenceWindow
	if w.timer != nil {
		w.timer.Stop()
	}
	top := w.maxObservedPriority
	release := func(list []pendingVote) {
		for _, p := range list {
			if settle {
				p.decide(p.priority >= top)
			} else {
				p.decide(false)
			}
		}
	}
	release(w.prevotePending)
	release(w.votePending)
	*w = electionWindow{}
}

// settlePreference closes the wait for an election at the deadline.
func (r *Raft) settlePreference(term, generation uint64) {
	w := &r.preferenceWindow
	if w.term != term || w.generation != generation || w.settled {
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	top := w.maxObservedPriority
	w.settled = true
	for _, p := range w.prevotePending {
		p.decide(p.priority >= top)
	}
	for _, p := range w.votePending {
		p.decide(p.priority >= top)
	}
	w.prevotePending = nil
	w.votePending = nil
	w.timer = nil
}

// resetPreference drops all preference state on (re)entering follower/leader.
func (r *Raft) resetPreference() {
	r.closePreferenceWindow(false)
	r.drainPreferenceKick()
}

func (r *Raft) drainPreferenceKick() {
	for {
		select {
		case <-r.preferenceKickCh:
		default:
			return
		}
	}
}

// kickPreferenceCampaign asks the follower loop to campaign immediately when
// a lower-priority server campaigns ahead of us.
func (r *Raft) kickPreferenceCampaign() {
	if r.getState() != Follower || !r.preferenceEnabled() || r.candidatePriority == 0 {
		return
	}
	if leaderAddr, _ := r.LeaderWithID(); leaderAddr != "" {
		return
	}
	select {
	case r.preferenceKickCh <- struct{}{}:
	default:
	}
}

// considerElectionPreference applies the bounded candidate preference to a
// vote or pre-vote request that has passed all ordinary eligibility checks.
// decide is invoked for rejected and deferred outcomes.
func (r *Raft) considerElectionPreference(
	term uint64,
	candidatePriority int,
	prevote bool,
	leadershipTransfer bool,
	decide func(grant bool),
) preferenceDecision {
	if !r.preferenceEnabled() || leadershipTransfer {
		return preferenceAnswerNow
	}

	w := r.windowFor(term)
	if candidatePriority > w.maxObservedPriority {
		w.maxObservedPriority = candidatePriority
	}

	// A candidate in this same election must neither defer nor veto a
	// competing campaign: doing so livelocks equal candidates. Pre-vote is
	// advisory and a real vote is still gated by one-vote-per-term.
	if r.getState() == Candidate &&
		(term == r.getCurrentTerm() || term == r.getCurrentTerm()+1) {
		return preferenceAnswerNow
	}

	// A preferred follower that sees a lower-priority candidate campaigns at
	// once; this is what lets a slightly-late preferred server still win.
	if r.candidatePriority > candidatePriority {
		r.kickPreferenceCampaign()
	}

	if w.settled || time.Now().After(w.deadline) {
		// The bounded wait elapsed during pre-vote.
		if prevote {
			// Pre-vote requests all had a chance to be observed: grant only
			// to the highest-priority candidates seen.
			decide(candidatePriority >= w.maxObservedPriority)
		} else {
			// For the binding vote the bounded wait has elapsed, so priority
			// no longer holds anything up; the ordinary one-vote-per-term and
			// log checks in the caller still apply.
			decide(true)
		}
		return preferenceRejected
	}

	p := pendingVote{priority: candidatePriority, decide: decide}
	if prevote {
		w.prevotePending = append(w.prevotePending, p)
	} else {
		w.votePending = append(w.votePending, p)
	}
	return preferenceDeferred
}

// settleRequest is sent when a preference window closes.
type settleRequest struct {
	term       uint64
	generation uint64
}
