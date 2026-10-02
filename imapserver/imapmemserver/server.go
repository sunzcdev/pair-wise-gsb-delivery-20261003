// Package imapmemserver implements an in-memory IMAP server.
package imapmemserver

import (
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Server is a server instance.
//
// A server contains a list of users.
type Server struct {
	mutex sync.Mutex
	users map[string]*User

	sharedMutex sync.Mutex
	shared      map[string]*Mailbox
}

// New creates a new server.
func New() *Server {
	return &Server{
		users:  make(map[string]*User),
		shared: make(map[string]*Mailbox),
	}
}

// NewSession creates a new IMAP session.
func (s *Server) NewSession() imapserver.Session {
	return &serverSession{server: s}
}

func (s *Server) user(username string) *User {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.users[username]
}

// AddUser adds a user to the server.
func (s *Server) AddUser(user *User) {
	user.server = s
	s.mutex.Lock()
	s.users[user.username] = user
	s.mutex.Unlock()
}

// CreateSharedMailbox creates a shared mailbox owned by the given user.
//
// Shared mailboxes are visible to all server users, but only the owner has
// rights by default. The owner can grant rights to other users with SETACL.
func (s *Server) CreateSharedMailbox(name, owner string) (*Mailbox, error) {
	s.sharedMutex.Lock()
	defer s.sharedMutex.Unlock()

	if s.shared[name] != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	}

	uidValidity := uint32(1)
	mbox := NewMailbox(name, uidValidity)
	mbox.initACL(owner)
	s.shared[name] = mbox
	return mbox, nil
}

func (s *Server) sharedMailbox(name string) *Mailbox {
	s.sharedMutex.Lock()
	defer s.sharedMutex.Unlock()
	return s.shared[name]
}

func (s *Server) sharedMailboxes() []*Mailbox {
	s.sharedMutex.Lock()
	defer s.sharedMutex.Unlock()

	l := make([]*Mailbox, 0, len(s.shared))
	for _, mbox := range s.shared {
		l = append(l, mbox)
	}
	return l
}

type serverSession struct {
	*UserSession // may be nil

	server *Server // immutable
}

var _ imapserver.Session = (*serverSession)(nil)

func (sess *serverSession) Login(username, password string) error {
	u := sess.server.user(username)
	if u == nil {
		return imapserver.ErrAuthFailed
	}
	if err := u.Login(username, password); err != nil {
		return err
	}
	sess.UserSession = NewUserSession(u)
	return nil
}
