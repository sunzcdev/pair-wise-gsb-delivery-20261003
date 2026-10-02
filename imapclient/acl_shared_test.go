package imapclient_test

import (
	"errors"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	ownerUsername = "owner"
	ownerPassword = "owner-pass"
	guestUsername = "guest"
	guestPassword = "guest-pass"

	sharedMailboxName = "Shared"
	destMailboxName   = "SharedDest"
)

// nonACLSession wraps an imapserver.Session while hiding the optional
// SessionACL interface, emulating a backend without ACL support.
type nonACLSession struct {
	imapserver.Session
}

var _ imapserver.Session = (*nonACLSession)(nil)

// Namespace and Move are delegated so the wrapper satisfies IMAP4rev2 while
// SessionACL stays hidden.
func (s *nonACLSession) Namespace() (*imap.NamespaceData, error) {
	return s.Session.(imapserver.SessionNamespace).Namespace()
}

func (s *nonACLSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	return s.Session.(imapserver.SessionMove).Move(w, numSet, dest)
}

type aclTestEnv struct {
	t      *testing.T
	mem    *imapmemserver.Server
	server *imapserver.Server
	ln     net.Listener
	owner  *imapclient.Client
	guest  *imapclient.Client
}

func newACLTestEnv(t *testing.T) *aclTestEnv {
	t.Helper()

	mem := imapmemserver.New()

	owner := imapmemserver.NewUser(ownerUsername, ownerPassword)
	if err := owner.Create("INBOX", nil); err != nil {
		t.Fatalf("owner Create INBOX: %v", err)
	}
	mem.AddUser(owner)

	guest := imapmemserver.NewUser(guestUsername, guestPassword)
	if err := guest.Create("INBOX", nil); err != nil {
		t.Fatalf("guest Create INBOX: %v", err)
	}
	mem.AddUser(guest)

	if _, err := mem.CreateSharedMailbox(sharedMailboxName, ownerUsername); err != nil {
		t.Fatalf("CreateSharedMailbox: %v", err)
	}
	if _, err := mem.CreateSharedMailbox(destMailboxName, ownerUsername); err != nil {
		t.Fatalf("CreateSharedMailbox dest: %v", err)
	}

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
			imap.CapACL:       {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go func() {
		_ = server.Serve(ln)
	}()

	env := &aclTestEnv{t: t, mem: mem, server: server, ln: ln}
	env.owner = env.dialLogin(ownerUsername, ownerPassword)
	env.guest = env.dialLogin(guestUsername, guestPassword)

	t.Cleanup(func() {
		env.owner.Close()
		env.guest.Close()
		server.Close()
	})

	return env
}

func (env *aclTestEnv) dialLogin(username, password string) *imapclient.Client {
	env.t.Helper()

	conn, err := net.Dial("tcp", env.ln.Addr().String())
	if err != nil {
		env.t.Fatalf("net.Dial: %v", err)
	}
	client := imapclient.New(conn, nil)
	if err := client.Login(username, password).Wait(); err != nil {
		env.t.Fatalf("Login %q: %v", username, err)
	}
	return client
}

func (env *aclTestEnv) append(client *imapclient.Client, mailbox string, flags ...imap.Flag) imap.UID {
	env.t.Helper()

	cmd := client.Append(mailbox, int64(len(simpleRawMessage)), &imap.AppendOptions{Flags: flags})
	if _, err := cmd.Write([]byte(simpleRawMessage)); err != nil {
		env.t.Fatalf("Append Write: %v", err)
	}
	if err := cmd.Close(); err != nil {
		env.t.Fatalf("Append Close: %v", err)
	}
	data, err := cmd.Wait()
	if err != nil {
		env.t.Fatalf("Append Wait: %v", err)
	}
	return data.UID
}

func (env *aclTestEnv) selectMailbox(client *imapclient.Client, mailbox string) {
	env.t.Helper()
	if _, err := client.Select(mailbox, nil).Wait(); err != nil {
		env.t.Fatalf("Select %q: %v", mailbox, err)
	}
}

func listMailboxNames(t *testing.T, client *imapclient.Client) map[string]struct{} {
	t.Helper()
	list, err := client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make(map[string]struct{}, len(list))
	for _, data := range list {
		names[data.Mailbox] = struct{}{}
	}
	return names
}

func expectIMAPError(t *testing.T, err error, code imap.ResponseCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected IMAP error with code %v, got nil", code)
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("expected *imap.Error, got %T: %v", err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Fatalf("expected NO response, got %v", imapErr.Type)
	}
	if imapErr.Code != code {
		t.Fatalf("expected response code %v, got %v (%q)", code, imapErr.Code, imapErr.Text)
	}
}

// TestACLSharedMailbox verifies ACL enforcement with two users and a shared
// mailbox. Permission changes are made while both sessions stay connected,
// exercising owner rights, positive grants and negative rights.
func TestACLSharedMailbox(t *testing.T) {
	env := newACLTestEnv(t)
	owner, guest := env.owner, env.guest

	if !owner.Caps().Has(imap.CapACL) {
		t.Fatal("server doesn't advertise ACL")
	}

	seedUID := env.append(owner, sharedMailboxName)

	t.Run("owner", func(t *testing.T) {
		data, err := owner.MyRights(sharedMailboxName).Wait()
		if err != nil {
			t.Fatalf("owner MyRights: %v", err)
		}
		for _, right := range imap.RightSetAll {
			if !data.Rights.Has(right) {
				t.Fatalf("owner missing right %q: %q", right, data.Rights)
			}
		}

		acl, err := owner.GetACL(sharedMailboxName).Wait()
		if err != nil {
			t.Fatalf("owner GetACL: %v", err)
		}
		ownerRights, ok := acl.Rights[imap.RightsIdentifier(ownerUsername)]
		if !ok {
			t.Fatalf("GetACL missing owner entry: %+v", acl.Rights)
		}
		for _, right := range imap.RightSetAll {
			if !ownerRights.Has(right) {
				t.Fatalf("GetACL owner rights %q missing %q", ownerRights, right)
			}
		}

		lr, err := owner.ListRights(sharedMailboxName, imap.RightsIdentifier(guestUsername)).Wait()
		if err != nil {
			t.Fatalf("owner ListRights: %v", err)
		}
		if lr.Mailbox != sharedMailboxName || lr.Identifier != imap.RightsIdentifier(guestUsername) {
			t.Fatalf("ListRights = %+v", lr)
		}
		if len(lr.Optional) == 0 {
			t.Fatal("ListRights returned no optional rights")
		}
	})

	t.Run("guest_invisible", func(t *testing.T) {
		if _, ok := listMailboxNames(t, guest)[sharedMailboxName]; ok {
			t.Fatal("guest can LIST shared mailbox without l")
		}

		_, err := guest.Status(sharedMailboxName, &imap.StatusOptions{NumMessages: true}).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		_, err = guest.Select(sharedMailboxName, nil).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		_, err = guest.MyRights(sharedMailboxName).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		cmd := guest.Append(sharedMailboxName, int64(len(simpleRawMessage)), nil)
		cmd.Write([]byte(simpleRawMessage))
		cmd.Close()
		_, err = cmd.Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		_, err = guest.GetACL(sharedMailboxName).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		err = guest.SetACL(sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet("a")).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)
	})

	// Positive grant: lookup only.
	setACL := func(client *imapclient.Client, mailbox string, ri imap.RightsIdentifier, mod imap.RightModification, rs imap.RightSet) {
		t.Helper()
		if err := client.SetACL(mailbox, ri, mod, rs).Wait(); err != nil {
			t.Fatalf("SetACL %q %q %q: %v", ri, mod, rs, err)
		}
	}
	setACL(owner, sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet("l"))

	t.Run("lookup_only", func(t *testing.T) {
		names := listMailboxNames(t, guest)
		if _, ok := names[sharedMailboxName]; !ok {
			t.Fatalf("guest cannot LIST shared mailbox after l grant: %+v", names)
		}

		my, err := guest.MyRights(sharedMailboxName).Wait()
		if err != nil {
			t.Fatalf("guest MyRights: %v", err)
		}
		if my.Rights.String() != "l" {
			t.Fatalf("guest rights = %q, want l", my.Rights)
		}

		// Visible but cannot read.
		_, err = guest.Select(sharedMailboxName, nil).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNoPerm)

		// GETACL and LISTRIGHTS are allowed with l.
		if _, err := guest.GetACL(sharedMailboxName).Wait(); err != nil {
			t.Fatalf("guest GetACL with l: %v", err)
		}
		if _, err := guest.ListRights(sharedMailboxName, imap.RightsIdentifier(guestUsername)).Wait(); err != nil {
			t.Fatalf("guest ListRights with l: %v", err)
		}

		// SETACL requires a.
		err = guest.SetACL(sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationAdd, imap.RightSet("r")).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNoPerm)

		// STATUS only needs l.
		status, err := guest.Status(sharedMailboxName, &imap.StatusOptions{NumMessages: true}).Wait()
		if err != nil {
			t.Fatalf("guest STATUS with l: %v", err)
		}
		if status.NumMessages == nil || *status.NumMessages != 1 {
			t.Fatalf("STATUS NumMessages = %v, want 1", status.NumMessages)
		}
	})

	setACL(owner, sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet("lrs"))

	t.Run("read_only", func(t *testing.T) {
		env.selectMailbox(guest, sharedMailboxName)

		msgs, err := guest.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
			Envelope: true,
		}).Collect()
		if err != nil {
			t.Fatalf("guest FETCH ENVELOPE with r: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("FETCH returned %d messages, want 1", len(msgs))
		}

		// Non-PEEK body fetch implicitly sets \Seen, which is allowed with s.
		if _, err := guest.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierNone}},
		}).Collect(); err != nil {
			t.Fatalf("guest FETCH BODY[] with rs: %v", err)
		}

		addFlags := func(flags ...imap.Flag) *imap.StoreFlags {
			return &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: flags}
		}
		// s only allows \Seen; \Deleted and other flags are denied.
		if err := guest.Store(imap.UIDSetNum(seedUID), addFlags(imap.FlagSeen), nil).Close(); err != nil {
			t.Fatalf("STORE \\Seen with s: %v", err)
		}
		expectIMAPError(t, guest.Store(imap.UIDSetNum(seedUID), addFlags(imap.FlagDeleted), nil).Close(), imap.ResponseCodeNoPerm)
		expectIMAPError(t, guest.Store(imap.UIDSetNum(seedUID), addFlags(imap.FlagFlagged), nil).Close(), imap.ResponseCodeNoPerm)

		// No i/d.
		deniedAppend := guest.Append(sharedMailboxName, 1, nil)
		deniedAppend.Write([]byte("x"))
		deniedAppend.Close()
		_, err = deniedAppend.Wait()
		expectIMAPError(t, err, imap.ResponseCodeNoPerm)
		expectIMAPError(t, guest.Expunge().Close(), imap.ResponseCodeNoPerm)

		// Destination exists but is invisible to the guest: COPY is denied
		// with NONEXISTENT so existence cannot be probed.
		_, err = guest.Copy(imap.UIDSetNum(seedUID), destMailboxName).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		// An unknown destination is indistinguishable from an invisible one.
		_, err = guest.Copy(imap.UIDSetNum(seedUID), "DoesNotExist").Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		// MOVE additionally needs d on the source, which is missing.
		_, err = guest.Move(imap.UIDSetNum(seedUID), destMailboxName).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNoPerm)
	})

	// Deliver, but not delete: grant i without d.
	setACL(owner, sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet("lrsi"))

	t.Run("insert_without_delete", func(t *testing.T) {
		uid := env.append(guest, sharedMailboxName)
		if uid == 0 {
			t.Fatal("APPEND returned UID 0")
		}

		expectIMAPError(t,
			guest.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(),
			imap.ResponseCodeNoPerm)
		expectIMAPError(t, guest.Expunge().Close(), imap.ResponseCodeNoPerm)
	})

	// Allow delivery into the destination mailbox, then COPY succeeds but
	// MOVE still needs d on the source.
	setACL(owner, destMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet("lri"))

	t.Run("copy_allowed_move_denied", func(t *testing.T) {
		if _, err := guest.Copy(imap.UIDSetNum(seedUID), destMailboxName).Wait(); err != nil {
			t.Fatalf("COPY with r+i: %v", err)
		}

		_, err := guest.Move(imap.UIDSetNum(seedUID), destMailboxName).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNoPerm)
	})

	setACL(owner, sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet("lrsidw"))

	t.Run("write_delete_granted", func(t *testing.T) {
		if err := guest.Store(imap.UIDSetNum(seedUID), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			t.Fatalf("STORE \\Deleted with d: %v", err)
		}

		if _, err := guest.Move(imap.UIDSetNum(seedUID), destMailboxName).Wait(); err != nil {
			t.Fatalf("MOVE with rd: %v", err)
		}

		if err := guest.Expunge().Close(); err != nil {
			t.Fatalf("EXPUNGE with d: %v", err)
		}
	})

	t.Run("negative_rights", func(t *testing.T) {
		// Reset the guest's explicit grant first, then make the mailbox
		// readable to everyone via "anyone".
		setACL(owner, sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet{})
		setACL(owner, sharedMailboxName, imap.RightsIdentifierAnyone, imap.RightModificationReplace, imap.RightSet("lr"))
		setACL(owner, sharedMailboxName, imap.RightsIdentifier("-"+imap.RightsIdentifier(guestUsername)), imap.RightModificationReplace, imap.RightSet("r"))

		my, err := guest.MyRights(sharedMailboxName).Wait()
		if err != nil {
			t.Fatalf("MyRights with negative: %v", err)
		}
		if my.Rights.Has(imap.RightRead) {
			t.Fatalf("negative r not applied: %q", my.Rights)
		}
		if !my.Rights.Has(imap.RightLookup) {
			t.Fatalf("negative removed l: %q", my.Rights)
		}

		_, err = guest.Select(sharedMailboxName, nil).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNoPerm)

		acl, err := owner.GetACL(sharedMailboxName).Wait()
		if err != nil {
			t.Fatalf("GetACL: %v", err)
		}
		neg := imap.RightsIdentifier("-" + guestUsername)
		if rs, ok := acl.Rights[neg]; !ok || rs.String() != "r" {
			t.Fatalf("negative entry wrong: %+v", acl.Rights)
		}

		if err := owner.DeleteACL(sharedMailboxName, neg).Wait(); err != nil {
			t.Fatalf("DeleteACL negative: %v", err)
		}
		my, err = guest.MyRights(sharedMailboxName).Wait()
		if err != nil {
			t.Fatalf("MyRights after negative removal: %v", err)
		}
		if !my.Rights.Has(imap.RightRead) {
			t.Fatalf("r not restored: %q", my.Rights)
		}

		err = owner.DeleteACL(sharedMailboxName, imap.RightsIdentifier("nobody-at-all")).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)
	})

	t.Run("revocation_hides", func(t *testing.T) {
		// Both the explicit and the "anyone" grants are removed on the
		// long-lived guest session.
		setACL(owner, sharedMailboxName, imap.RightsIdentifier(guestUsername), imap.RightModificationReplace, imap.RightSet{})
		setACL(owner, sharedMailboxName, imap.RightsIdentifierAnyone, imap.RightModificationReplace, imap.RightSet{})

		if _, ok := listMailboxNames(t, guest)[sharedMailboxName]; ok {
			t.Fatal("guest can still LIST shared mailbox after revocation")
		}

		_, err := guest.MyRights(sharedMailboxName).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)

		_, err = guest.Status(sharedMailboxName, &imap.StatusOptions{NumMessages: true}).Wait()
		expectIMAPError(t, err, imap.ResponseCodeNonExistent)
	})
}

// TestACLNonACLBackend checks that a backend not implementing SessionACL
// keeps its original capability set instead of gaining a fake all-allowing
// ACL implementation.
func TestACLNonACLBackend(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUsername, testPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create INBOX: %v", err)
	}
	mem.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &nonACLSession{Session: mem.NewSession()}, nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer server.Close()
	go server.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	client := imapclient.New(conn, nil)
	defer client.Close()

	if client.Caps().Has(imap.CapACL) {
		t.Fatal("non-ACL backend advertises the ACL capability")
	}

	if err := client.Login(testUsername, testPassword).Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Core operations keep working.
	cmd := client.Append("INBOX", int64(len(simpleRawMessage)), nil)
	cmd.Write([]byte(simpleRawMessage))
	cmd.Close()
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true}).Collect(); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// ACL commands are rejected when the backend doesn't implement them.
	_, err = client.MyRights("INBOX").Wait()
	expectIMAPErrorOrBad(t, err)
}

func expectIMAPErrorOrBad(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error for unsupported ACL command, got nil")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("expected *imap.Error, got %T: %v", err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo && imapErr.Type != imap.StatusResponseTypeBad {
		t.Fatalf("expected NO/BAD, got %v", imapErr.Type)
	}
}
