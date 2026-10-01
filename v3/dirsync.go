package ldap

import (
	"errors"
	"fmt"
)

// ErrDirSyncUnconfirmedBatch is returned by DirSyncSession.Next when the
// previously delivered batch has not been confirmed yet. A batch must be
// confirmed via DirSyncBatch.Confirm before the next one can be requested.
var ErrDirSyncUnconfirmedBatch = errors.New("ldap: previous dirsync batch has not been confirmed")

// ErrDirSyncBaselineRequired is returned by DirSyncSession.Next when the
// server rejects the current cookie. The sync position is lost and a new
// baseline must be established (see DirSyncSession.Reset); the result must
// not be treated as "no changes".
var ErrDirSyncBaselineRequired = errors.New("ldap: dirsync cookie rejected by server, a new baseline sync is required")

// DirSyncState is the persistable state of a DirSyncSession. It binds the
// sync position (Cookie) to the server identity, base DN and filter the
// position was established with, so a state is never resumed against a
// different server or query by accident.
type DirSyncState struct {
	// Server identifies the server the cookie was obtained from, e.g. its
	// "host:port" address.
	Server string
	// BaseDN is the base DN of the search the cookie belongs to.
	BaseDN string
	// Filter is the search filter the cookie belongs to.
	Filter string
	// Cookie is the last confirmed server cookie. It is empty before the
	// first batch has been confirmed.
	Cookie []byte
}

// DirSyncSession runs a confirmed-delivery sync on top of the DirSync
// control. Each batch of entries is handed to the caller, and the
// persistable cookie only advances once the caller confirms the batch has
// been saved. A session is not safe for concurrent use.
type DirSyncSession struct {
	client        Client
	searchRequest *SearchRequest
	control       *ControlDirSync
	flags         int64
	maxAttrCount  int64

	state         DirSyncState
	pendingCookie []byte
	hasPending    bool
	done          bool
}

// NewDirSyncSession creates a sync session for the given search request.
// server is a stable identifier of the server being synced (e.g. its
// "host:port" address); it becomes part of the persistable state.
//
// If state is not nil, the session resumes from it. The state must have
// been produced by a session with the same server, base DN and filter,
// otherwise an error is returned. If the search request does not contain a
// DirSync control yet, one is added with the given flags and maxAttrCount.
func NewDirSyncSession(
	client Client, server string, searchRequest *SearchRequest, flags, maxAttrCount int64, state *DirSyncState,
) (*DirSyncSession, error) {
	if client == nil {
		return nil, errors.New("ldap: dirsync session requires a client")
	}
	if searchRequest == nil {
		return nil, errors.New("ldap: dirsync session requires a search request")
	}

	s := &DirSyncSession{
		client:        client,
		searchRequest: searchRequest,
		flags:         flags,
		maxAttrCount:  maxAttrCount,
		state: DirSyncState{
			Server: server,
			BaseDN: searchRequest.BaseDN,
			Filter: searchRequest.Filter,
		},
	}

	if state != nil {
		if state.Server != server {
			return nil, fmt.Errorf("ldap: dirsync state is bound to server %q, cannot resume against %q", state.Server, server)
		}
		if state.BaseDN != searchRequest.BaseDN {
			return nil, fmt.Errorf("ldap: dirsync state is bound to base DN %q, cannot resume against %q", state.BaseDN, searchRequest.BaseDN)
		}
		if state.Filter != searchRequest.Filter {
			return nil, fmt.Errorf("ldap: dirsync state is bound to filter %q, cannot resume against %q", state.Filter, searchRequest.Filter)
		}
		s.state.Cookie = append([]byte(nil), state.Cookie...)
	}

	switch control := FindControl(searchRequest.Controls, ControlTypeDirSync).(type) {
	case nil:
		s.control = NewRequestControlDirSync(flags, maxAttrCount, s.state.Cookie)
		searchRequest.Controls = append(searchRequest.Controls, s.control)
	case *ControlDirSync:
		if control.Flags != flags {
			return nil, fmt.Errorf("flags given in search request (%d) conflicts with flags given in session (%d)", control.Flags, flags)
		}
		if control.MaxAttrCount != maxAttrCount {
			return nil, fmt.Errorf("MaxAttrCnt given in search request (%d) conflicts with maxAttrCount given in session (%d)", control.MaxAttrCount, maxAttrCount)
		}
		s.control = control
	default:
		return nil, fmt.Errorf("ldap: expected dirsync control to be of type *ControlDirSync, got %v", control)
	}

	return s, nil
}

// Next fetches the next batch of changed entries from the server. The
// returned cookie stays pending until the caller confirms the batch via
// DirSyncBatch.Confirm; calling Next again before confirming returns
// ErrDirSyncUnconfirmedBatch.
//
// If the server rejects the current cookie, Next returns an error matching
// ErrDirSyncBaselineRequired and the caller must re-establish a baseline
// instead of treating the result as "no changes".
//
// Once the server signals that no more changes are available and the final
// batch has been confirmed, Next returns (nil, nil).
func (s *DirSyncSession) Next() (*DirSyncBatch, error) {
	if s.hasPending {
		return nil, ErrDirSyncUnconfirmedBatch
	}
	if s.done {
		return nil, nil
	}

	s.control.SetCookie(s.state.Cookie)
	result, err := s.client.DirSync(s.searchRequest, s.flags, s.maxAttrCount, s.state.Cookie)
	if err != nil {
		if IsErrorWithCode(err, LDAPResultUnwillingToPerform) {
			return nil, fmt.Errorf("%w: %w", ErrDirSyncBaselineRequired, err)
		}
		return nil, err
	}
	if result == nil {
		return nil, NewError(ErrorNetwork, errors.New("ldap: packet not received"))
	}

	resultControl, ok := FindControl(result.Controls, ControlTypeDirSync).(*ControlDirSync)
	if !ok {
		return nil, errors.New("ldap: server did not return a dirsync response control")
	}

	s.pendingCookie = append([]byte(nil), resultControl.Cookie...)
	s.hasPending = true

	return &DirSyncBatch{
		session:   s,
		Entries:   result.Entries,
		Referrals: result.Referrals,
		Controls:  result.Controls,
	}, nil
}

// State returns the persistable state of the session. The cookie it carries
// only covers confirmed batches; an unconfirmed batch is never reflected in
// the state, so a restarted session safely repeats that batch.
func (s *DirSyncSession) State() DirSyncState {
	return DirSyncState{
		Server: s.state.Server,
		BaseDN: s.state.BaseDN,
		Filter: s.state.Filter,
		Cookie: append([]byte(nil), s.state.Cookie...),
	}
}

// Done reports whether the server signaled that no more changes are
// available and the final batch has been confirmed.
func (s *DirSyncSession) Done() bool {
	return s.done
}

// Reset discards the current cookie so the next call to Next establishes a
// new baseline. It is typically called after Next reported
// ErrDirSyncBaselineRequired. Reset fails with ErrDirSyncUnconfirmedBatch
// if a batch is still waiting for confirmation.
func (s *DirSyncSession) Reset() error {
	if s.hasPending {
		return ErrDirSyncUnconfirmedBatch
	}
	s.state.Cookie = nil
	s.done = false
	return nil
}

func (s *DirSyncSession) confirm() {
	s.state.Cookie = append([]byte(nil), s.pendingCookie...)
	s.pendingCookie = nil
	s.hasPending = false
	if len(s.state.Cookie) == 0 {
		s.done = true
	}
}

// DirSyncBatch is a single batch of changed entries delivered by
// DirSyncSession.Next.
type DirSyncBatch struct {
	session *DirSyncSession

	// Entries are the changed entries of this batch.
	Entries []*Entry
	// Referrals are the referrals returned with this batch.
	Referrals []string
	// Controls are the controls returned with this batch.
	Controls []Control

	confirmed bool
}

// Confirm marks the batch as durably saved by the caller. Only then does
// the session's persistable state advance to the cookie of this batch.
// Confirm is idempotent.
func (b *DirSyncBatch) Confirm() {
	if b == nil || b.confirmed {
		return
	}
	b.session.confirm()
	b.confirmed = true
}
