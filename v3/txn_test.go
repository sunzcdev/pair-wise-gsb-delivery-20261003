package ldap

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txnMockConn is an in-memory net.Conn that lets a test play an LDAP server:
// received request packets are decoded and pushed onto a channel, and the
// test sends response packets back through SendResponse.
type txnMockConn struct {
	mu      sync.Mutex
	closed  bool
	respBuf bytes.Buffer
	reqBuf  bytes.Buffer
	respC   *sync.Cond
	reqC    *sync.Cond
}

func newTxnMockConn() *txnMockConn {
	c := &txnMockConn{}
	c.respC = sync.NewCond(&c.mu)
	c.reqC = sync.NewCond(&c.mu)
	return c
}

func (c *txnMockConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for !c.closed {
		n, err := c.respBuf.Read(b)
		if err != io.EOF {
			return n, err
		}
		c.respC.Wait()
	}
	return 0, io.EOF
}

func (c *txnMockConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	defer c.reqC.Broadcast()
	return c.reqBuf.Write(b)
}

func (c *txnMockConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.respC.Broadcast()
		c.reqC.Broadcast()
	}
	return nil
}

func (c *txnMockConn) LocalAddr() net.Addr              { return (*net.TCPAddr)(nil) }
func (c *txnMockConn) RemoteAddr() net.Addr             { return (*net.TCPAddr)(nil) }
func (c *txnMockConn) SetDeadline(time.Time) error      { return nil }
func (c *txnMockConn) SetReadDeadline(time.Time) error  { return nil }
func (c *txnMockConn) SetWriteDeadline(time.Time) error { return nil }

// SendResponse queues a BER packet for the client reader.
func (c *txnMockConn) SendResponse(p *ber.Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.respBuf.Write(p.Bytes())
	c.respC.Broadcast()
}

// recvRequest pops one decoded request from the mock connection.
func (c *txnMockConn) recvRequest(t *testing.T) *ber.Packet {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for !c.closed {
		r := bytes.NewReader(c.reqBuf.Bytes())
		p, err := ber.ReadPacket(r)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			c.reqC.Wait()
			continue
		}
		require.NoError(t, err)
		c.reqBuf.Next(c.reqBuf.Len() - r.Len())
		return p
	}
	t.Fatal("mock connection closed before request arrived")
	return nil
}

// waitRequest blocks until the next request is available or fails after d.
func (c *txnMockConn) waitRequest(t *testing.T, d time.Duration) *ber.Packet {
	t.Helper()
	done := make(chan *ber.Packet, 1)
	go func() { done <- c.recvRequest(t) }()
	select {
	case p := <-done:
		return p
	case <-time.After(d):
		t.Fatal("timed out waiting for request")
		return nil
	}
}

func newTxnConn(mock *txnMockConn) *Conn {
	conn := NewConn(mock, false)
	conn.SetTimeout(5 * time.Second)
	conn.Start()
	return conn
}

// ---- BER request inspection helpers -------------------------------------

func reqMessageID(t *testing.T, p *ber.Packet) int64 {
	t.Helper()
	id, err := packetInt64At(p, 0)
	require.NoError(t, err)
	return id
}

func reqProtocolOp(t *testing.T, p *ber.Packet) *ber.Packet {
	t.Helper()
	op, err := packetChild(p, 1)
	require.NoError(t, err)
	return op
}

// extendedRequestName returns the requestName ([0] OID string) of an extended
// request packet.
func extendedRequestName(t *testing.T, op *ber.Packet) string {
	t.Helper()
	child, err := packetChild(op, 0)
	require.NoError(t, err)
	data, err := packetData(child)
	require.NoError(t, err)
	return string(data)
}

// extendedRequestValueData returns the raw contents of the optional
// requestValue [1] field, or nil when absent.
func extendedRequestValueData(t *testing.T, op *ber.Packet) []byte {
	t.Helper()
	for _, ch := range op.Children {
		if ch.ClassType == ber.ClassContext && ch.Tag == ber.Tag(1) {
			data, err := packetData(ch)
			require.NoError(t, err)
			return data
		}
	}
	return nil
}

// requestTxnIdentifier extracts the transaction identifier carried by the
// critical Transaction Specification control of a request, or nil when the
// control is absent.
func requestTxnIdentifier(t *testing.T, p *ber.Packet) []byte {
	t.Helper()
	controls, ok, err := packetChildIfPresent(p, 2)
	require.NoError(t, err)
	if !ok {
		return nil
	}
	for _, ctrl := range controls.Children {
		typeChild, err := packetChild(ctrl, 0)
		require.NoError(t, err)
		oidData, err := packetData(typeChild)
		require.NoError(t, err)
		oid := string(oidData)
		if oid != ControlTypeTxnSpec {
			continue
		}
		// Remaining children are criticality BOOLEAN and controlValue.
		for _, ch := range ctrl.Children[1:] {
			if ch.ClassType == ber.ClassUniversal && ch.Tag == ber.TagBoolean {
				assert.True(t, ch.Value.(bool), "transaction specification control must be critical")
				continue
			}
			id, err := packetData(ch)
			require.NoError(t, err)
			return id
		}
	}
	return nil
}

// ---- BER response builders ----------------------------------------------

// newLDAPResult builds the protocol op and its envelope, attaching the op to
// the envelope only after it is fully populated so that any extra children
// appended by the caller-to-be are actually serialized.
func newLDAPResult(messageID int64, appTag ber.Tag, resultCode uint16, matchedDN, diagnostic string) (*ber.Packet, *ber.Packet) {
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, appTag, nil, ApplicationMap[uint8(appTag)])
	op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(resultCode), "resultCode"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, matchedDN, "matchedDN"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, diagnostic, "diagnosticMessage"))
	env := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	env.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, messageID, "MessageID"))
	return env, op
}

func ldapResultEnvelope(messageID int64, appTag ber.Tag, resultCode uint16, matchedDN, diagnostic string) *ber.Packet {
	env, op := newLDAPResult(messageID, appTag, resultCode, matchedDN, diagnostic)
	env.AppendChild(op)
	return env
}

func modifyResponse(messageID int64, resultCode uint16, diagnostic string) *ber.Packet {
	return ldapResultEnvelope(messageID, ApplicationModifyResponse, resultCode, "", diagnostic)
}

func addResponse(messageID int64, resultCode uint16, diagnostic string) *ber.Packet {
	return ldapResultEnvelope(messageID, ApplicationAddResponse, resultCode, "", diagnostic)
}

func startTxnResponse(messageID int64, identifier []byte) *ber.Packet {
	env, op := newLDAPResult(messageID, ApplicationExtendedResponse, LDAPResultSuccess, "", "")
	value := ber.Encode(ber.ClassContext, ber.TypePrimitive, ber.TagEmbeddedPDV, nil, "responseValue")
	value.Data.Write(identifier)
	op.AppendChild(value)
	env.AppendChild(op)
	return env
}

func startTxnErrorResponse(messageID int64, resultCode uint16, diagnostic string) *ber.Packet {
	return ldapResultEnvelope(messageID, ApplicationExtendedResponse, resultCode, "", diagnostic)
}

// endTxnResponse builds an End Transaction extended response. failedMessageID
// < 0 omits the responseValue; otherwise it is encoded as
// SEQUENCE { messageID MessageID }.
func endTxnResponse(messageID int64, resultCode uint16, diagnostic string, failedMessageID int64) *ber.Packet {
	env, op := newLDAPResult(messageID, ApplicationExtendedResponse, resultCode, "", diagnostic)
	if failedMessageID >= 0 {
		seq := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "EndTransactionResponseValue")
		seq.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, failedMessageID, "messageID"))
		value := ber.Encode(ber.ClassContext, ber.TypePrimitive, ber.TagEmbeddedPDV, nil, "responseValue")
		value.Data.Write(seq.Bytes())
		op.AppendChild(value)
	}
	env.AppendChild(op)
	return env
}

func searchResultDone(messageID int64) *ber.Packet {
	return ldapResultEnvelope(messageID, ApplicationSearchResultDone, LDAPResultSuccess, "", "")
}

func searchResultEntry(messageID int64, dn string, attrType, attrValue string) *ber.Packet {
	env := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	env.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, messageID, "MessageID"))
	entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationSearchResultEntry, nil, "Search Result Entry")
	entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, dn, "objectName"))
	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes")
	partial := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "PartialAttribute")
	partial.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, attrType, "type"))
	values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "values")
	values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, attrValue, "value"))
	partial.AppendChild(values)
	attrs.AppendChild(partial)
	entry.AppendChild(attrs)
	env.AppendChild(entry)
	return env
}

// txnIdentifier builds the contents of an OCTET STRING carrying a BER-encoded
// transaction identifier (here a simple INTEGER, as the bytes are opaque).
func txnIdentifier(v int64) []byte {
	id := ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, v, "transaction identifier")
	return id.Bytes()
}

func assertTxnRequest(t *testing.T, p *ber.Packet, appTag ber.Tag, wantIdentifier []byte) {
	t.Helper()
	op := reqProtocolOp(t, p)
	assert.Equal(t, appTag, op.Tag, "unexpected application operation")
	assert.Equal(t, wantIdentifier, requestTxnIdentifier(t, p), "request must carry the transaction identifier")
}

// TestTxnCommitFlow drives a full Start -> Modify/Add -> Commit exchange and
// verifies every request is sent on the one connection with the correct
// critical Transaction Specification control and that the commit completes.
func TestTxnCommitFlow(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(7)

	var startID, modifyID, addID, commitID int64
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		// Start Transaction: extended request with no value.
		start := mock.recvRequest(t)
		startOp := reqProtocolOp(t, start)
		assert.Equal(t, OIDStartTransaction, extendedRequestName(t, startOp))
		assert.Nil(t, extendedRequestValueData(t, startOp))
		startID = reqMessageID(t, start)
		mock.SendResponse(startTxnResponse(startID, identifier))

		// Modify within the transaction.
		modify := mock.recvRequest(t)
		assertTxnRequest(t, modify, ApplicationModifyRequest, identifier)
		modifyID = reqMessageID(t, modify)
		mock.SendResponse(modifyResponse(modifyID, LDAPResultSuccess, ""))

		// Add within the transaction.
		add := mock.recvRequest(t)
		assertTxnRequest(t, add, ApplicationAddRequest, identifier)
		addID = reqMessageID(t, add)
		mock.SendResponse(addResponse(addID, LDAPResultSuccess, ""))

		// Commit: End Transaction with an empty value (commit defaults TRUE).
		commit := mock.recvRequest(t)
		commitOp := reqProtocolOp(t, commit)
		assert.Equal(t, OIDEndTransaction, extendedRequestName(t, commitOp))
		valueData := extendedRequestValueData(t, commitOp)
		require.NotNil(t, valueData)
		valuePkt, err := ber.DecodePacketErr(valueData)
		require.NoError(t, err)
		assert.Equal(t, 0, len(valuePkt.Children), "commit must omit the commit field (DEFAULT TRUE)")
		commitID = reqMessageID(t, commit)
		mock.SendResponse(endTxnResponse(commitID, LDAPResultSuccess, "", -1))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)
	assert.True(t, txn.Active())
	assert.Equal(t, identifier, txn.Identifier())

	modifyReq := NewModifyRequest("cn=alice,dc=example,dc=org", nil)
	modifyReq.Replace("description", []string{"in txn"})
	require.NoError(t, txn.Modify(modifyReq))

	addReq := NewAddRequest("cn=bob,dc=example,dc=org", nil)
	addReq.Attribute("cn", []string{"bob"})
	require.NoError(t, txn.Add(addReq))

	result, err := txn.Commit()
	require.NoError(t, err)
	assert.True(t, result.Committed)
	assert.False(t, result.HasFailedMessageID)
	assert.False(t, txn.Active())

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish the exchange")
	}

	// A committed handle cannot be reused.
	err = txn.Modify(modifyReq)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTxnCommitted), "got %v", err)
	_, err = txn.Commit()
	assert.True(t, errors.Is(err, ErrTxnCommitted), "got %v", err)
}

// TestTxnBoundToOriginatingConnection proves the handle keeps using the
// connection that created it: a second connection sees no transaction traffic
// even while the first one carries every exchange.
func TestTxnBoundToOriginatingConnection(t *testing.T) {
	mock1 := newTxnMockConn()
	mock2 := newTxnMockConn()
	conn1 := newTxnConn(mock1)
	conn2 := newTxnConn(mock2)
	defer func() { _ = conn1.Close() }()
	defer func() { _ = conn2.Close() }()

	identifier := txnIdentifier(9)

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		start := mock1.recvRequest(t)
		mock1.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))

		modify := mock1.recvRequest(t)
		assertTxnRequest(t, modify, ApplicationModifyRequest, identifier)
		mock1.SendResponse(modifyResponse(reqMessageID(t, modify), LDAPResultSuccess, ""))

		end := mock1.recvRequest(t)
		assert.Equal(t, OIDEndTransaction, extendedRequestName(t, reqProtocolOp(t, end)))
		mock1.SendResponse(endTxnResponse(reqMessageID(t, end), LDAPResultSuccess, "", -1))
	}()

	txn, err := conn1.StartTxn()
	require.NoError(t, err)

	// All operations are issued while conn2 is alive; the handle must never
	// route them there.
	require.NoError(t, txn.Modify(NewModifyRequest("cn=carol,dc=example,dc=org", nil)))
	_, err = txn.Commit()
	require.NoError(t, err)

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("originating server did not finish")
	}

	// mock2 must have received no request bytes.
	mock2.mu.Lock()
	assert.Equal(t, 0, mock2.reqBuf.Len(), "transaction traffic leaked onto another connection")
	mock2.mu.Unlock()
}

// TestTxnInterleavedSearch verifies message attribution when the server
// interleaves ordinary search responses with transaction operation responses.
// The search reply must reach Search while the modify reply reaches the
// transaction operation, despite both requests being outstanding.
func TestTxnInterleavedSearch(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(3)

	var txnModifyID, searchID int64
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))

		// Wait until both the transaction modify and the ordinary search are
		// outstanding, then answer in the opposite order and interleave the
		// search entry with the modify result.
		modify := mock.recvRequest(t)
		search := mock.waitRequest(t, 2*time.Second)

		assertTxnRequest(t, modify, ApplicationModifyRequest, identifier)
		assert.Equal(t, ber.Tag(ApplicationSearchRequest), reqProtocolOp(t, search).Tag)

		txnModifyID = reqMessageID(t, modify)
		searchID = reqMessageID(t, search)

		// Interleave: search entry, modify result, then search done.
		mock.SendResponse(searchResultEntry(searchID, "cn=alice,dc=example,dc=org", "cn", "alice"))
		mock.SendResponse(modifyResponse(txnModifyID, LDAPResultSuccess, ""))
		mock.SendResponse(searchResultDone(searchID))

		// Answer the commit only after the End request arrives, so it is
		// attributed to a known outstanding message.
		end := mock.recvRequest(t)
		assert.Equal(t, OIDEndTransaction, extendedRequestName(t, reqProtocolOp(t, end)))
		mock.SendResponse(endTxnResponse(reqMessageID(t, end), LDAPResultSuccess, "", -1))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)

	modifyReq := NewModifyRequest("cn=alice,dc=example,dc=org", nil)
	modifyReq.Replace("mail", []string{"alice@example.com"})

	modifyErr := make(chan error, 1)
	go func() { modifyErr <- txn.Modify(modifyReq) }()

	// Give the modify a moment to become outstanding, then issue an ordinary
	// search on the same connection.
	time.Sleep(100 * time.Millisecond)
	searchReq := NewSearchRequest("dc=example,dc=org", ScopeBaseObject, NeverDerefAliases, 0, 0, false, "(objectClass=*)", []string{"cn"}, nil)
	result, searchErr := conn.Search(searchReq)

	require.NoError(t, searchErr)
	require.Len(t, result.Entries, 1)
	assert.Equal(t, "cn=alice,dc=example,dc=org", result.Entries[0].DN)
	assert.Equal(t, "alice", result.Entries[0].GetAttributeValue("cn"))

	require.NoError(t, <-modifyErr)

	// The transaction remains usable and commits normally.
	_, err = txn.Commit()
	require.NoError(t, err)

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish")
	}
}

// TestTxnServerInitiatedAbort covers the server terminating the transaction on
// its own by sending an unsolicited Notice of Disconnection and dropping the
// connection. Every handle method afterwards reports an aborted transaction,
// and the failure detail is retained.
func TestTxnServerInitiatedAbort(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(5)
	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))

		// Wait for the first in-transaction update, then abort out of band.
		_ = mock.recvRequest(t)

		// An unsolicited notification carries messageID 0 and responseName
		// [10], followed by the usual LDAPResult fields.
		notice, noticeOp := newLDAPResult(0, ApplicationExtendedResponse, LDAPResultUnavailable, "", "transaction aborted by server")
		name := ber.NewString(ber.ClassContext, ber.TypePrimitive, ber.Tag(10), "1.3.6.1.4.1.1466.20036", "responseName")
		noticeOp.AppendChild(name)
		notice.AppendChild(noticeOp)
		mock.SendResponse(notice)
		_ = mock.Close()
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)

	modifyReq := NewModifyRequest("cn=alice,dc=example,dc=org", nil)
	modifyReq.Replace("mail", []string{"a@example.com"})
	// The update itself fails because the connection is torn down.
	err = txn.Modify(modifyReq)
	require.Error(t, err)
	require.Error(t, txn.LastError())

	// Give the connection state a moment to observe the disconnect, then the
	// handle must be unusable in every direction.
	deadline := time.Now().Add(2 * time.Second)
	for txn.Active() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.False(t, txn.Active(), "transaction should be marked aborted after the server disconnected")

	err = txn.Modify(modifyReq)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTxnAborted), "expected aborted error, got %v", err)

	_, err = txn.Commit()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTxnAborted), "expected aborted error on commit, got %v", err)

	err = txn.Abort()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTxnAborted), "expected aborted error on abort, got %v", err)
}

// TestTxnCommitDisconnectInDoubt covers the connection breaking while a commit
// is outstanding: the server's decision is unknowable, so Commit must report
// an explicit in-doubt outcome and the handle stays unusable.
func TestTxnCommitDisconnectInDoubt(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(11)
	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))

		modify := mock.recvRequest(t)
		mock.SendResponse(modifyResponse(reqMessageID(t, modify), LDAPResultSuccess, ""))

		// Read the commit and close without answering.
		_ = mock.recvRequest(t)
		_ = mock.Close()
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)
	require.NoError(t, txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil)))

	_, err = txn.Commit()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTxnInDoubt), "expected in-doubt error, got %v", err)
	assert.False(t, txn.Active())
	require.Error(t, txn.LastError())
	assert.True(t, errors.Is(txn.LastError(), ErrTxnInDoubt))

	// Further use keeps surfacing the in-doubt result rather than pretending
	// the transaction is committed or merely aborted.
	_, err = txn.Commit()
	assert.True(t, errors.Is(err, ErrTxnInDoubt), "got %v", err)
	err = txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil))
	assert.True(t, errors.Is(err, ErrTxnInDoubt), "got %v", err)
}

// TestTxnCallerAbort covers the caller explicitly aborting: the End Transaction
// request carries commit=FALSE, the server confirms, and the handle is dead.
func TestTxnCallerAbort(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(13)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))

		modify := mock.recvRequest(t)
		mock.SendResponse(modifyResponse(reqMessageID(t, modify), LDAPResultSuccess, ""))

		end := mock.recvRequest(t)
		endOp := reqProtocolOp(t, end)
		assert.Equal(t, OIDEndTransaction, extendedRequestName(t, endOp))
		valueData := extendedRequestValueData(t, endOp)
		require.NotNil(t, valueData)
		valuePkt, err := ber.DecodePacketErr(valueData)
		require.NoError(t, err)
		require.Len(t, valuePkt.Children, 1)
		commitChild := valuePkt.Children[0]
		assert.Equal(t, ber.ClassContext, commitChild.ClassType)
		assert.Equal(t, ber.Tag(1), commitChild.Tag)
		commitData, err := packetData(commitChild)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x00}, commitData, "commit [1] BOOLEAN must encode FALSE as 0x00")
		mock.SendResponse(endTxnResponse(reqMessageID(t, end), LDAPResultSuccess, "", -1))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)
	require.NoError(t, txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil)))

	require.NoError(t, txn.Abort())
	assert.False(t, txn.Active())

	// After abort every method is rejected.
	err = txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil))
	assert.True(t, errors.Is(err, ErrTxnAborted), "got %v", err)
	_, err = txn.Commit()
	assert.True(t, errors.Is(err, ErrTxnAborted), "got %v", err)
	assert.True(t, errors.Is(txn.Abort(), ErrTxnAborted))

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish")
	}
}

// TestTxnFailedOperationInfoPreserved verifies that a failed update inside a
// transaction returns its LDAP result detail, leaves the transaction usable,
// and that a failed commit surfaces both the LDAP error and the failed
// operation's message ID returned by the server.
func TestTxnFailedOperationInfoPreserved(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(17)
	failedModifyID := int64(-1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))

		// First update fails with an LDAP result code; the transaction must
		// stay open.
		failed := mock.recvRequest(t)
		failedModifyID = reqMessageID(t, failed)
		mock.SendResponse(modifyResponse(failedModifyID, LDAPResultNoSuchObject, "entry does not exist"))

		// A subsequent update on the same transaction still works.
		next := mock.recvRequest(t)
		assertTxnRequest(t, next, ApplicationModifyRequest, identifier)
		mock.SendResponse(modifyResponse(reqMessageID(t, next), LDAPResultSuccess, ""))

		// Commit fails; the server identifies the failing operation.
		commit := mock.recvRequest(t)
		mock.SendResponse(endTxnResponse(reqMessageID(t, commit), LDAPResultOperationsError, "one update failed", failedModifyID))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)

	err = txn.Modify(NewModifyRequest("cn=ghost,dc=example,dc=org", nil))
	require.Error(t, err)
	var ldapErr *Error
	require.True(t, errors.As(err, &ldapErr), "expected *ldap.Error, got %T", err)
	assert.Equal(t, uint16(LDAPResultNoSuchObject), ldapErr.ResultCode)
	assert.Contains(t, ldapErr.Err.Error(), "entry does not exist")
	assert.True(t, txn.Active(), "an LDAP result-code failure must not abort the transaction")
	assert.Same(t, ldapErr, asLDAPError(t, txn.LastError()))

	// The failed operation information is retained and still observable.
	require.NoError(t, txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil)))

	result, err := txn.Commit()
	require.Error(t, err)
	require.NotNil(t, result)
	assert.True(t, errors.As(err, &ldapErr))
	assert.Equal(t, uint16(LDAPResultOperationsError), ldapErr.ResultCode)
	assert.True(t, result.HasFailedMessageID)
	assert.Equal(t, failedModifyID, result.FailedMessageID)
	assert.False(t, txn.Active())

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not finish")
	}
}

func asLDAPError(t *testing.T, err error) *Error {
	t.Helper()
	var ldapErr *Error
	require.True(t, errors.As(err, &ldapErr))
	return ldapErr
}

// TestTxnEndResponseWithoutMessageID confirms a transaction-level End failure
// (for example an unknown transaction) carries no failed message ID.
func TestTxnEndResponseWithoutMessageID(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(19)
	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))
		commit := mock.recvRequest(t)
		mock.SendResponse(endTxnResponse(reqMessageID(t, commit), LDAPResultUnwillingToPerform, "unknown transaction", -1))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)
	result, err := txn.Commit()
	require.Error(t, err)
	require.NotNil(t, result)
	assert.False(t, result.HasFailedMessageID)
	assert.True(t, IsErrorWithCode(err, LDAPResultUnwillingToPerform))
	assert.False(t, txn.Active())
}

// TestTxnCommitMalformedResponseInDoubt covers an End Transaction response
// that arrives but cannot be parsed as an extended response. The server's
// decision is unknowable, so a commit must be reported as in doubt while an
// abort is a definite failure.
func TestTxnCommitMalformedResponseInDoubt(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	identifier := txnIdentifier(27)
	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), identifier))
		end := mock.recvRequest(t)
		// A bind response is not an extended response, so decoding fails
		// structurally while still being a readable packet.
		mock.SendResponse(ldapResultEnvelope(reqMessageID(t, end), ApplicationBindResponse, LDAPResultSuccess, "", ""))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)
	result, err := txn.Commit()
	require.Error(t, err)
	require.NotNil(t, result)
	assert.False(t, result.HasFailedMessageID)
	assert.True(t, errors.Is(err, ErrTxnInDoubt), "expected in-doubt error, got %v", err)
	assert.False(t, txn.Active())
}

// TestStartTxnServerRejects surfaces an LDAP error from Start Transaction and
// does not return a handle.
func TestStartTxnServerRejects(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnErrorResponse(reqMessageID(t, start), LDAPResultUnwillingToPerform, "transactions unsupported"))
	}()

	txn, err := conn.StartTxn()
	assert.Nil(t, txn)
	require.Error(t, err)
	assert.True(t, IsErrorWithCode(err, LDAPResultUnwillingToPerform), "got %v", err)
}

// TestStartTxnMissingIdentifier rejects a success response that omits the
// required responseValue.
func TestStartTxnMissingIdentifier(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(ldapResultEnvelope(reqMessageID(t, start), ApplicationExtendedResponse, LDAPResultSuccess, "", ""))
	}()

	txn, err := conn.StartTxn()
	assert.Nil(t, txn)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMalformedPacket), "got %v", err)
}

// TestStartTxnDisconnect reports a network error rather than a handle.
func TestStartTxnDisconnect(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	go func() {
		_ = mock.recvRequest(t)
		_ = mock.Close()
	}()

	txn, err := conn.StartTxn()
	assert.Nil(t, txn)
	require.Error(t, err)
	// A torn read surfaces the underlying I/O error; it need not be wrapped
	// in an *Error with code ErrorNetwork, but it must never be nil.
	var ldapErr *Error
	if errors.As(err, &ldapErr) {
		assert.Equal(t, uint16(ErrorNetwork), ldapErr.ResultCode)
	}
}

// TestControlTxnSpecRoundTrip verifies the critical Transaction Specification
// control encodes the identifier as the contents of its OCTET STRING value and
// survives DecodeControl.
func TestControlTxnSpecRoundTrip(t *testing.T) {
	identifier := txnIdentifier(23)
	control := NewControlTxnSpec(identifier)
	assert.Equal(t, ControlTypeTxnSpec, control.GetControlType())

	encoded := control.Encode()
	require.Len(t, encoded.Children, 3)
	assert.Equal(t, ControlTypeTxnSpec, encoded.Children[0].Data.String())
	crit, err := packetBool(encoded.Children[1])
	require.NoError(t, err)
	assert.True(t, crit)
	assert.Equal(t, identifier, encoded.Children[2].Data.Bytes())

	decoded, err := DecodeControl(encoded)
	require.NoError(t, err)
	txnControl, ok := decoded.(*ControlTxnSpec)
	require.True(t, ok)
	assert.Equal(t, identifier, txnControl.Identifier)
}

// TestEncodeEndTxnRequestValue checks the commit field encoding for both
// commit (absent, DEFAULT TRUE) and abort ([1] BOOLEAN FALSE).
func TestEncodeEndTxnRequestValue(t *testing.T) {
	commit := encodeEndTxnRequestValue(true)
	assert.Equal(t, 0, len(commit.Children))

	abort := encodeEndTxnRequestValue(false)
	require.Len(t, abort.Children, 1)
	child := abort.Children[0]
	assert.Equal(t, ber.ClassContext, child.ClassType)
	assert.Equal(t, ber.TypePrimitive, child.TagType)
	assert.Equal(t, ber.Tag(1), child.Tag)
	b, err := packetBool(child)
	require.NoError(t, err)
	assert.False(t, b)
}

// TestTxnNilRequests ensures nil operation requests are rejected without
// touching the wire and without changing the transaction state.
func TestTxnNilRequests(t *testing.T) {
	mock := newTxnMockConn()
	conn := newTxnConn(mock)
	defer func() { _ = conn.Close() }()

	go func() {
		start := mock.recvRequest(t)
		mock.SendResponse(startTxnResponse(reqMessageID(t, start), txnIdentifier(29)))
	}()

	txn, err := conn.StartTxn()
	require.NoError(t, err)

	assert.Error(t, txn.Add(nil))
	assert.Error(t, txn.Del(nil))
	assert.Error(t, txn.Modify(nil))
	assert.Error(t, txn.ModifyDN(nil))
	assert.True(t, txn.Active())
}
