// Package imapmemserver implements an in-memory IMAP server.
package imapmemserver

import (
	"sync"

	"github.com/emersion/go-imap/v2/imapserver"
)

// Server is a server instance.
//
// A server contains a list of users.
type Server struct {
	mutex sync.Mutex
	users map[string]*User
}

// New creates a new server.
func New() *Server {
	return &Server{
		users: make(map[string]*User),
	}
}

// NewSession creates a new IMAP session.
func (s *Server) NewSession() imapserver.Session {
	return &serverSession{server: s}
}

// NewAdminSession creates a new IMAP session with quota-administration
// privileges.
//
// It authenticates against the same user accounts as NewSession: the
// distinction is the privilege granted after authentication. A deployment
// would expose NewAdminSession on a separate, access-controlled listener.
// A user authenticated through such a session can issue SETQUOTA; the same
// user on a regular session cannot.
func (s *Server) NewAdminSession() imapserver.Session {
	return &adminServerSession{server: s}
}

func (s *Server) user(username string) *User {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.users[username]
}

// AddUser adds a user to the server.
func (s *Server) AddUser(user *User) {
	s.mutex.Lock()
	s.users[user.username] = user
	s.mutex.Unlock()
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

type adminServerSession struct {
	*AdminSession // may be nil

	server *Server // immutable
}

func (sess *adminServerSession) Close() error {
	if sess.AdminSession != nil {
		return sess.AdminSession.Close()
	}
	return nil
}

func (sess *adminServerSession) Login(username, password string) error {
	u := sess.server.user(username)
	if u == nil {
		return imapserver.ErrAuthFailed
	}
	if err := u.Login(username, password); err != nil {
		return err
	}
	sess.AdminSession = &AdminSession{UserSession: NewUserSession(u)}
	return nil
}
