package ldap

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrDirSyncCookieRejected is returned by DirSyncSession.Next when the server
// rejects the cookie held by the session (e.g. because it is too old or the
// server lost its replication state). The caller must re-establish a baseline
// by starting a new session without state; the failed poll must not be
// treated as "no changes".
var ErrDirSyncCookieRejected = errors.New("ldap: dirsync cookie rejected by server, baseline must be re-established")

// ErrDirSyncStateMismatch is returned by NewDirSyncSession when a previously
// persisted DirSyncState does not match the server identity, base DN or
// search criteria of the new session.
var ErrDirSyncStateMismatch = errors.New("ldap: dirsync state does not match the current server or search request")

// ErrDirSyncUncommittedBatch is returned by DirSyncSession.Next when the
// previous batch was delivered but not yet confirmed with Commit.
var ErrDirSyncUncommittedBatch = errors.New("ldap: previous dirsync batch has not been committed")

// ErrDirSyncControlMissing is returned by DirSyncSession.Next when the server
// response does not include a DirSync response control, so no trustworthy
// cookie can be derived from it.
var ErrDirSyncControlMissing = errors.New("ldap: server response does not include a dirsync control")

// DirSyncState is the persistable checkpoint of a DirSyncSession. It binds
// the sync cookie to the identity of the server and to the exact search
// criteria it was obtained with, so a state is never silently reused for a
// different server, base DN or query.
type DirSyncState struct {
	Server     string   `json:"server"`
	BaseDN     string   `json:"baseDN"`
	Scope      int      `json:"scope"`
	Filter     string   `json:"filter"`
	Attributes []string `json:"attributes"`
	Flags      int64    `json:"flags"`
	Cookie     []byte   `json:"cookie"`
}

// MarshalDirSyncState serializes a DirSyncState for persistent storage.
func MarshalDirSyncState(state *DirSyncState) ([]byte, error) {
	return json.Marshal(state)
}

// UnmarshalDirSyncState restores a DirSyncState previously serialized with
// MarshalDirSyncState.
func UnmarshalDirSyncState(data []byte) (*DirSyncState, error) {
	var state DirSyncState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("ldap: failed to decode dirsync state: %w", err)
	}
	return &state, nil
}

// DirSyncBatch is one page of changed entries delivered by DirSyncSession.Next.
// The batch must be confirmed with Commit once the caller has durably saved
// the entries; only then does the session's persistable state advance.
type DirSyncBatch struct {
	Entries   []*Entry
	Referrals []string
	// Done reports that the server returned no further entries for the
	// current cookie, i.e. the caller is up to date.
	Done bool
}

// DirSyncSession runs a cookie-based DirSync conversation on top of the
// DirSync control. Each call to Next delivers one batch of entries; the new
// cookie becomes part of the persistable State only after the caller confirms
// the batch with Commit. If the process restarts before Commit, the last
// persisted state still holds the previous cookie, so the interrupted batch
// is delivered again (at-least-once) instead of being skipped.
type DirSyncSession struct {
	client       Client
	server       string
	request      *SearchRequest
	flags        int64
	maxAttrCount int64
	control      *ControlDirSync

	cookie     []byte
	pending    []byte
	hasPending bool
}

// NewDirSyncSession creates a session over client. server is an opaque
// identity string for the server (e.g. its host:port) that is recorded in the
// persisted state. If state is non-nil, the session resumes from it, but only
// if the state was recorded for the same server, base DN and search criteria;
// otherwise ErrDirSyncStateMismatch is returned and a baseline must be
// established by creating a session without state.
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
		client:       client,
		server:       server,
		request:      searchRequest,
		flags:        flags,
		maxAttrCount: maxAttrCount,
	}
	if state != nil {
		if err := s.checkState(state); err != nil {
			return nil, err
		}
		s.cookie = append([]byte(nil), state.Cookie...)
	}

	if control := FindControl(searchRequest.Controls, ControlTypeDirSync); control != nil {
		c, ok := control.(*ControlDirSync)
		if !ok {
			return nil, fmt.Errorf("ldap: search request contains unexpected dirsync control type %T", control)
		}
		if c.Flags != flags {
			return nil, fmt.Errorf("flags given in search request (%d) conflicts with flags given in session (%d)", c.Flags, flags)
		}
		if c.MaxAttrCount != maxAttrCount {
			return nil, fmt.Errorf("MaxAttrCnt given in search request (%d) conflicts with maxAttrCount given in session (%d)", c.MaxAttrCount, maxAttrCount)
		}
		s.control = c
	} else {
		s.control = NewRequestControlDirSync(flags, maxAttrCount, nil)
		searchRequest.Controls = append(searchRequest.Controls, s.control)
	}
	s.control.SetCookie(s.cookie)
	return s, nil
}

// checkState verifies that a persisted state belongs to this server and
// search request.
func (s *DirSyncSession) checkState(state *DirSyncState) error {
	mismatch := func(field string) error {
		return fmt.Errorf("%w: %s differs", ErrDirSyncStateMismatch, field)
	}
	if state.Server != s.server {
		return mismatch("server")
	}
	if state.BaseDN != s.request.BaseDN {
		return mismatch("base DN")
	}
	if state.Scope != s.request.Scope {
		return mismatch("scope")
	}
	if state.Filter != s.request.Filter {
		return mismatch("filter")
	}
	if state.Flags != s.flags {
		return mismatch("flags")
	}
	want := append([]string(nil), s.request.Attributes...)
	got := append([]string(nil), state.Attributes...)
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) {
		return mismatch("attributes")
	}
	for i := range want {
		if want[i] != got[i] {
			return mismatch("attributes")
		}
	}
	return nil
}

// Next fetches the next batch of changed entries from the server. It fails
// with ErrDirSyncUncommittedBatch if the previous batch was not confirmed
// with Commit, and with ErrDirSyncCookieRejected if the server refuses the
// session's cookie, in which case a new baseline must be established.
func (s *DirSyncSession) Next() (*DirSyncBatch, error) {
	if s.hasPending {
		return nil, ErrDirSyncUncommittedBatch
	}
	s.control.SetCookie(s.cookie)
	result, err := s.client.Search(s.request)
	if err != nil {
		if len(s.cookie) > 0 && IsErrorWithCode(err, LDAPResultUnwillingToPerform) {
			return nil, fmt.Errorf("%w: %w", ErrDirSyncCookieRejected, err)
		}
		return nil, err
	}
	if result == nil {
		return nil, NewError(ErrorNetwork, errors.New("ldap: packet not received"))
	}
	control := FindControl(result.Controls, ControlTypeDirSync)
	if control == nil {
		return nil, ErrDirSyncControlMissing
	}
	dirSyncControl, ok := control.(*ControlDirSync)
	if !ok {
		return nil, fmt.Errorf("ldap: unexpected dirsync response control type %T", control)
	}
	// Keep the new cookie pending; it only becomes persistable via Commit.
	// An empty response cookie means the server did not advance the cookie.
	s.pending = s.cookie
	if len(dirSyncControl.Cookie) > 0 {
		s.pending = append([]byte(nil), dirSyncControl.Cookie...)
	}
	s.hasPending = true
	return &DirSyncBatch{
		Entries:   result.Entries,
		Referrals: result.Referrals,
		Done:      len(result.Entries) == 0,
	}, nil
}

// Commit confirms that the entries of the last batch delivered by Next have
// been durably saved. Only after Commit does State include the new cookie.
func (s *DirSyncSession) Commit() {
	if !s.hasPending {
		return
	}
	s.cookie = s.pending
	s.pending = nil
	s.hasPending = false
}

// State returns the persistable state of the session. It only ever contains
// confirmed cookies: a cookie received from the server becomes visible here
// solely after the corresponding batch was confirmed with Commit.
func (s *DirSyncSession) State() *DirSyncState {
	attributes := append([]string(nil), s.request.Attributes...)
	sort.Strings(attributes)
	return &DirSyncState{
		Server:     s.server,
		BaseDN:     s.request.BaseDN,
		Scope:      s.request.Scope,
		Filter:     s.request.Filter,
		Attributes: attributes,
		Flags:      s.flags,
		Cookie:     append([]byte(nil), s.cookie...),
	}
}
