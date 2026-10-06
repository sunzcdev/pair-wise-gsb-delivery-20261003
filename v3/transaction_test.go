package ldap

import (
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTxnIdentifier = "txn-1"

// testLDAPResult builds an application-class response carrying an LDAPResult.
func testLDAPResult(appTag uint8, code uint16, diag string) *ber.Packet {
	result := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ber.Tag(appTag), nil, "Response")
	result.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated,
		int64(code), "resultCode"))
	result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString,
		"", "matchedDN"))
	result.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString,
		diag, "diagnosticMessage"))
	return result
}

// testResponse wraps a protocol op in an LDAP message envelope addressed to
// msgID, attaching controls at the envelope level where LDAP places them.
func testResponse(msgID int64, protocolOp *ber.Packet, controls []Control) *ber.Packet {
	envelope := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	envelope.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger,
		msgID, "MessageID"))
	envelope.AppendChild(protocolOp)
	if len(controls) > 0 {
		envelope.AppendChild(encodeControls(controls))
	}
	return envelope
}

// testTxnControl is the echoed Transaction Specification Control servers put
// on in-transaction update responses.
func testTxnControl() *ControlTransaction {
	return &ControlTransaction{Identifier: []byte(testTxnIdentifier)}
}

// testStartTxnResponse builds a successful Start Transaction extended response
// carrying the identifier in responseValue [11].
func testStartTxnResponse(msgID int64) *ber.Packet {
	ext := testLDAPResult(ApplicationExtendedResponse, LDAPResultSuccess, "")
	value := ber.Encode(ber.ClassContext, ber.TypePrimitive, 11, nil, "responseValue")
	value.Data.Write([]byte(testTxnIdentifier))
	ext.AppendChild(value)
	return testResponse(msgID, ext, nil)
}

// testUpdateResponse builds a successful update response echoing the
// transaction control at the envelope level.
func testUpdateResponse(appTag uint8, msgID int64) *ber.Packet {
	return testResponse(msgID, testLDAPResult(appTag, LDAPResultSuccess, ""),
		[]Control{testTxnControl()})
}

// testEndTxnResponse builds an End Transaction extended response. failedValue
// is an already encoded EndTxnResponseValue appended as responseValue [11].
func testEndTxnResponse(msgID int64, code uint16, diag string, failedValue *ber.Packet) *ber.Packet {
	ext := testLDAPResult(ApplicationExtendedResponse, code, diag)
	if failedValue != nil {
		ext.AppendChild(failedValue)
	}
	return testResponse(msgID, ext, nil)
}

// testSearchEntry builds a SearchResultEntry with one attribute.
func testSearchEntry(msgID int64, dn string) *ber.Packet {
	entry := ber.Encode(ber.ClassApplication, ber.TypeConstructed,
		ber.Tag(ApplicationSearchResultEntry), nil, "Search Result Entry")
	entry.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, dn, "DN"))
	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attributes")
	attr := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Attribute")
	attr.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "cn", "Type"))
	values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "Vals")
	values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString,
		"alice", "Value"))
	attr.AppendChild(values)
	attrs.AppendChild(attr)
	entry.AppendChild(attrs)
	return testResponse(msgID, entry, nil)
}

// testSearchDone builds a SearchResultDone. It deliberately carries no
// transaction control, as a normal search is not part of the transaction.
func testSearchDone(msgID int64) *ber.Packet {
	return testResponse(msgID,
		testLDAPResult(ApplicationSearchResultDone, LDAPResultSuccess, ""), nil)
}

// testFailedOpsValue builds the EndTxnResponseValue OCTET STRING responseValue
// describing the given failed message IDs, each with an embedded add response
// carrying the supplied result code and message.
func testFailedOpsValue(failures map[int64]uint16, msgs map[int64]string) *ber.Packet {
	outer := ber.Encode(ber.ClassContext, ber.TypePrimitive, 11, nil, "responseValue")
	seq := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil,
		"End Transaction Response Value")
	for messageID, code := range failures {
		element := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Failed op")
		element.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger,
			messageID, "messageID"))
		element.AppendChild(testLDAPResult(ApplicationAddResponse, code, msgs[messageID]))
		seq.AppendChild(element)
	}
	outer.AppendChild(seq)
	return outer
}

// testTxnServer serves one in-memory connection. Every incoming request is
// handed to handler, which returns the response packets to write back.
type testTxnServer struct {
	ptc     *packetTranslatorConn
	conn    *Conn
	mu      sync.Mutex
	closed  bool
	handler func(req *ber.Packet) []*ber.Packet
}

func newTestTxnServer(t *testing.T) *testTxnServer {
	t.Helper()
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	server := &testTxnServer{ptc: ptc, conn: conn}

	go func() {
		for {
			req, err := ptc.ReceiveRequest()
			if err != nil {
				return
			}
			server.mu.Lock()
			closed := server.closed
			handler := server.handler
			server.mu.Unlock()
			if closed {
				return
			}
			responses := handler(req)
			for _, resp := range responses {
				if resp != nil {
					_ = ptc.SendResponse(resp)
				}
			}
		}
	}()

	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})
	return server
}

func (s *testTxnServer) requestMsgID(req *ber.Packet) int64 {
	msgID, err := packetInt64At(req, 0)
	if err != nil {
		return -1
	}
	return msgID
}

// disconnect closes the in-memory connection as a server would after aborting
// a transaction, causing the client reader to fail.
func (s *testTxnServer) disconnect() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	_ = s.ptc.Close()
}

// requestControls decodes the controls attached to a request envelope.
func requestControls(t *testing.T, req *ber.Packet) []Control {
	t.Helper()
	controlsChild, ok, err := packetChildIfPresent(req, 2)
	require.NoError(t, err)
	if !ok {
		return nil
	}
	controls := make([]Control, 0, len(controlsChild.Children))
	for _, child := range controlsChild.Children {
		control, err := DecodeControl(child)
		require.NoError(t, err)
		controls = append(controls, control)
	}
	return controls
}

// extendedRequestInfo extracts the requestName [0] and, when present, the
// decoded requestValue [1] from an ExtendedRequest protocol op.
func extendedRequestInfo(t *testing.T, protocolOp *ber.Packet) (name string, value *ber.Packet) {
	t.Helper()
	for _, child := range protocolOp.Children {
		switch {
		case child.ClassType == ber.ClassContext && child.Tag == 0:
			data, err := packetData(child)
			require.NoError(t, err)
			name = string(data)
		case child.ClassType == ber.ClassContext && child.Tag == 1:
			inner, err := ber.DecodePacketErr(child.Data.Bytes())
			require.NoError(t, err)
			value = inner
		}
	}
	return name, value
}

// recordedRequest captures what the test server received for one message.
type recordedRequest struct {
	msgID      int64
	appTag     ber.Tag
	protocolOp *ber.Packet
	controls   []Control
}

// standardTxnHandler recognizes Start Transaction, in-transaction updates,
// End Transaction and Search requests, recording each and returning the
// response a conforming RFC 5805 server would.
func standardTxnHandler(t *testing.T, server *testTxnServer, requests *[]recordedRequest) func(*ber.Packet) []*ber.Packet {
	t.Helper()
	return func(req *ber.Packet) []*ber.Packet {
		msgID := server.requestMsgID(req)
		protocolOp := req.Children[1]
		recorded := recordedRequest{
			msgID:      msgID,
			appTag:     protocolOp.Tag,
			protocolOp: protocolOp,
			controls:   requestControls(t, req),
		}

		switch protocolOp.Tag {
		case ber.Tag(ApplicationExtendedRequest):
			name, _ := extendedRequestInfo(t, protocolOp)
			if name != extendedOperationNameTransaction {
				return nil
			}
			*requests = append(*requests, recorded)
			// The first extended operation seen starts a transaction; later
			// ones end it.
			if len(*requests) == 1 {
				return []*ber.Packet{testStartTxnResponse(msgID)}
			}
			return []*ber.Packet{testEndTxnResponse(msgID, LDAPResultSuccess, "", nil)}
		case ber.Tag(ApplicationAddRequest):
			*requests = append(*requests, recorded)
			return []*ber.Packet{testUpdateResponse(ApplicationAddResponse, msgID)}
		case ber.Tag(ApplicationModifyRequest):
			*requests = append(*requests, recorded)
			return []*ber.Packet{testUpdateResponse(ApplicationModifyResponse, msgID)}
		case ber.Tag(ApplicationDelRequest):
			*requests = append(*requests, recorded)
			return []*ber.Packet{testUpdateResponse(ApplicationDelResponse, msgID)}
		case ber.Tag(ApplicationModifyDNRequest):
			*requests = append(*requests, recorded)
			return []*ber.Packet{testUpdateResponse(ApplicationModifyDNResponse, msgID)}
		case ber.Tag(ApplicationSearchRequest):
			*requests = append(*requests, recorded)
			return []*ber.Packet{testSearchDone(msgID)}
		default:
			return nil
		}
	}
}

func txnControlOf(t *testing.T, controls []Control) *ControlTransaction {
	t.Helper()
	control := FindControl(controls, ControlTypeTransaction)
	require.NotNil(t, control, "expected transaction specification control")
	txn, ok := control.(*ControlTransaction)
	require.True(t, ok, "expected *ControlTransaction, got %T", control)
	return txn
}

// TestTransactionCommitSuccess drives a full Start -> Add/Modify/Del/ModifyDN
// -> Commit sequence and checks the requests carry the transaction control
// with the server-assigned identifier and criticality true.
func TestTransactionCommitSuccess(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = standardTxnHandler(t, server, &requests)

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NotNil(t, txn)
	assert.Equal(t, []byte(testTxnIdentifier), txn.Identifier())

	add := NewAddRequest("cn=alice,dc=example,dc=org", nil)
	add.Attribute("objectClass", []string{"person"})
	add.Attribute("cn", []string{"alice"})
	require.NoError(t, txn.Add(add))

	modify := NewModifyRequest("cn=alice,dc=example,dc=org", nil)
	modify.Replace("sn", []string{"smith"})
	require.NoError(t, txn.Modify(modify))

	require.NoError(t, txn.Del(NewDelRequest("cn=bob,dc=example,dc=org", nil)))

	moddn := NewModifyDNRequest("cn=carol,dc=example,dc=org", "cn=carol2", true, "")
	require.NoError(t, txn.ModifyDN(moddn))

	// Exactly one Start and no End yet.
	require.Len(t, requests, 5)

	require.NoError(t, txn.Commit())
	require.Len(t, requests, 6)
	assert.True(t, txn.Finished())
	assert.False(t, txn.Aborted())

	// The four updates are keyed by their message IDs.
	for _, recorded := range requests[1:5] {
		txnControl := txnControlOf(t, recorded.controls)
		assert.True(t, txnControl.Criticality, "message %d control must be critical", recorded.msgID)
		assert.Equal(t, []byte(testTxnIdentifier), txnControl.Identifier)

		op := txn.Operation(recorded.msgID)
		require.NotNil(t, op)
		assert.Equal(t, recorded.msgID, op.MessageID)
	}

	// Start Transaction carries no transaction control; End carries none
	// either (it uses the extended request value).
	assert.Nil(t, FindControl(requests[0].controls, ControlTypeTransaction))
	assert.Nil(t, FindControl(requests[5].controls, ControlTypeTransaction))
}

// TestTransactionPreservesCallerControls verifies caller-supplied controls on
// a staged request survive while a duplicate transaction control supplied by
// the caller is replaced by the authoritative handle control.
func TestTransactionPreservesCallerControls(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = standardTxnHandler(t, server, &requests)

	txn, err := conn.StartTransaction()
	require.NoError(t, err)

	manage := NewControlManageDsaIT(true)
	rogue := &ControlTransaction{Criticality: false, Identifier: []byte("rogue")}
	add := NewAddRequest("cn=alice,dc=example,dc=org", []Control{manage, rogue})
	require.NoError(t, txn.Add(add))

	require.Len(t, requests, 2)
	update := requests[1]
	require.Len(t, update.controls, 2)
	assert.NotNil(t, FindControl(update.controls, ControlTypeManageDsaIT))
	txnControl := txnControlOf(t, update.controls)
	assert.True(t, txnControl.Criticality)
	assert.Equal(t, []byte(testTxnIdentifier), txnControl.Identifier)

	// The caller's original request must not be mutated.
	require.Len(t, add.Controls, 2)
}

// TestTransactionEndRequestValue checks the End Transaction extended request
// name and value (commit=true, identifier) for Commit.
func TestTransactionEndRequestValue(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = standardTxnHandler(t, server, &requests)

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Commit())

	end := requests[len(requests)-1].protocolOp
	name, value := extendedRequestInfo(t, end)
	assert.Equal(t, extendedOperationNameTransaction, name)
	require.NotNil(t, value)
	require.Len(t, value.Children, 2)

	commitData, err := packetData(value.Children[0])
	require.NoError(t, err)
	commit := commitData[0] != 0
	assert.True(t, commit)

	identifier, err := packetData(value.Children[1])
	require.NoError(t, err)
	assert.Equal(t, []byte(testTxnIdentifier), identifier)
}

// TestTransactionInterleavedSearch routes responses by message ID even when the
// server interleaves a transaction update and an ordinary search: the update
// response (with the transaction control) and the search entry/done (without
// it) must each reach their own outstanding request and not be misattributed.
func TestTransactionInterleavedSearch(t *testing.T) {
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})

	go func() {
		// 1. Start Transaction.
		start, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		startID := packetMustMsgID(t, start)
		_ = ptc.SendResponse(testStartTxnResponse(startID))

		// 2. Receive the in-transaction add but hold its response.
		add, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		addID := packetMustMsgID(t, add)

		// 3. Receive the ordinary search before answering the add.
		search, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		searchID := packetMustMsgID(t, search)

		// 4. Answer the search first (entry + done, no transaction control).
		_ = ptc.SendResponse(testSearchEntry(searchID, "cn=alice,dc=example,dc=org"))
		_ = ptc.SendResponse(testSearchDone(searchID))

		// 5. Then answer the add with the echoed transaction control.
		_ = ptc.SendResponse(testUpdateResponse(ApplicationAddResponse, addID))

		// 6. End transaction.
		end, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = ptc.SendResponse(testEndTxnResponse(packetMustMsgID(t, end), LDAPResultSuccess, "", nil))
	}()

	txn, err := conn.StartTransaction()
	require.NoError(t, err)

	addDone := make(chan error, 1)
	go func() {
		addDone <- txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	}()

	// Give the add time to be in flight, then issue the ordinary search on the
	// same connection.
	assert.Eventually(t, func() bool {
		txn.mu.Lock()
		defer txn.mu.Unlock()
		return len(txn.operations) == 1
	}, time.Second, 5*time.Millisecond)

	search := NewSearchRequest("dc=example,dc=org", ScopeWholeSubtree, NeverDerefAliases,
		0, 0, false, "(cn=alice)", nil, nil)
	result, err := conn.Search(search)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	assert.Equal(t, "cn=alice,dc=example,dc=org", result.Entries[0].DN)
	assert.Empty(t, result.Controls)

	select {
	case err := <-addDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("transaction add did not complete after interleaved search")
	}

	require.NoError(t, txn.Commit())
	assert.False(t, txn.Aborted())
}

func packetMustMsgID(t *testing.T, req *ber.Packet) int64 {
	t.Helper()
	msgID, err := packetInt64At(req, 0)
	require.NoError(t, err)
	return msgID
}

// TestTransactionServerAbortsMidway covers the server actively terminating the
// transaction by disconnecting after an update request. The update returns a
// network error and the handle is marked aborted; both Commit and Abort then
// report ErrTransactionAborted without touching the dead connection.
func TestTransactionServerAbortsMidway(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = func(req *ber.Packet) []*ber.Packet {
		handler := standardTxnHandler(t, server, &requests)
		msgID := server.requestMsgID(req)
		protocolOp := req.Children[1]
		if protocolOp.Tag == ber.Tag(ApplicationAddRequest) {
			requests = append(requests, recordedRequest{
				msgID: msgID, appTag: protocolOp.Tag, protocolOp: protocolOp,
				controls: requestControls(t, req),
			})
			// Terminate the transaction by dropping the connection without
			// answering the update.
			go server.disconnect()
			return nil
		}
		return handler(req)
	}

	txn, err := conn.StartTransaction()
	require.NoError(t, err)

	err = txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	require.Error(t, err)
	assert.True(t, txn.Aborted())

	// The underlying LDAP error must remain available in the chain.
	assert.True(t, IsErrorWithCode(err, ErrorNetwork))

	err = txn.Commit()
	assert.ErrorIs(t, err, ErrTransactionAborted)

	err = txn.Abort()
	assert.ErrorIs(t, err, ErrTransactionFinished)

	// Once Commit ended the handle, further updates and end calls are rejected
	// as finished rather than aborted.
	err = txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil))
	assert.ErrorIs(t, err, ErrTransactionFinished)
}

// TestTransactionCommitDisconnectUncertain covers the wire breaking while the
// End Transaction (commit) is outstanding. The client cannot know whether the
// server committed, so the error must wrap ErrTransactionCommitUncertain and
// the handle must be finished.
func TestTransactionCommitDisconnectUncertain(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = func(req *ber.Packet) []*ber.Packet {
		handler := standardTxnHandler(t, server, &requests)
		protocolOp := req.Children[1]
		if protocolOp.Tag == ber.Tag(ApplicationExtendedRequest) &&
			len(requests) >= 1 {
			requests = append(requests, recordedRequest{
				msgID: server.requestMsgID(req), appTag: protocolOp.Tag, protocolOp: protocolOp,
			})
			go server.disconnect()
			return nil
		}
		return handler(req)
	}

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil)))

	err = txn.Commit()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTransactionCommitUncertain)
	assert.True(t, txn.Finished())

	// The handle is finished, so reuse reports ErrTransactionFinished rather
	// than another uncertain outcome.
	err = txn.Commit()
	assert.ErrorIs(t, err, ErrTransactionFinished)
}

// TestTransactionCallerAbort covers the caller explicitly aborting: Commit is
// false in the End request value, Abort returns nil and the handle is marked
// finished and aborted.
func TestTransactionCallerAbort(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = func(req *ber.Packet) []*ber.Packet {
		handler := standardTxnHandler(t, server, &requests)
		// Only the End request is special: this test aborts, so answer success.
		return handler(req)
	}

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil)))

	require.NoError(t, txn.Abort())
	assert.True(t, txn.Finished())
	assert.True(t, txn.Aborted())

	end := requests[len(requests)-1].protocolOp
	name, value := extendedRequestInfo(t, end)
	assert.Equal(t, extendedOperationNameTransaction, name)
	require.NotNil(t, value)
	require.Len(t, value.Children, 2)
	commitData, err := packetData(value.Children[0])
	require.NoError(t, err)
	commit := commitData[0] != 0
	assert.False(t, commit)

	// Handle reuse after an abort is rejected.
	err = txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	assert.ErrorIs(t, err, ErrTransactionFinished)
	err = txn.Abort()
	assert.ErrorIs(t, err, ErrTransactionFinished)
	err = txn.Commit()
	assert.ErrorIs(t, err, ErrTransactionFinished)
}

// TestTransactionHandleReuseAfterCommit verifies the handle cannot be used
// after a successful commit.
func TestTransactionHandleReuseAfterCommit(t *testing.T) {
	var requests []recordedRequest
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = standardTxnHandler(t, server, &requests)

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Commit())

	err = txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	assert.ErrorIs(t, err, ErrTransactionFinished)
	err = txn.Modify(NewModifyRequest("cn=alice,dc=example,dc=org", nil))
	assert.ErrorIs(t, err, ErrTransactionFinished)
	err = txn.Del(NewDelRequest("cn=alice,dc=example,dc=org", nil))
	assert.ErrorIs(t, err, ErrTransactionFinished)
	moddn := NewModifyDNRequest("cn=alice,dc=example,dc=org", "cn=alice2", true, "")
	err = txn.ModifyDN(moddn)
	assert.ErrorIs(t, err, ErrTransactionFinished)
}

// TestTransactionCommitFailureRetainsOperations checks that when the server
// refuses to commit, the TransactionResultError retains the End result and the
// per-operation failure information, and the message IDs map back to the
// operations the caller staged.
func TestTransactionCommitFailureRetainsOperations(t *testing.T) {
	var addMsgID, secondAddMsgID int64
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = func(req *ber.Packet) []*ber.Packet {
		msgID := server.requestMsgID(req)
		protocolOp := req.Children[1]
		switch protocolOp.Tag {
		case ber.Tag(ApplicationExtendedRequest):
			if addMsgID == 0 {
				return []*ber.Packet{testStartTxnResponse(msgID)}
			}
			// End Transaction: refuse commit and report both failed adds.
			failures := map[int64]uint16{
				addMsgID:       LDAPResultConstraintViolation,
				secondAddMsgID: LDAPResultEntryAlreadyExists,
			}
			msgs := map[int64]string{
				addMsgID:       "constraint",
				secondAddMsgID: "already exists",
			}
			return []*ber.Packet{testEndTxnResponse(msgID, LDAPResultOperationsError,
				"transaction failed", testFailedOpsValue(failures, msgs))}
		case ber.Tag(ApplicationAddRequest):
			if addMsgID == 0 {
				addMsgID = msgID
			} else {
				secondAddMsgID = msgID
			}
			return []*ber.Packet{testUpdateResponse(ApplicationAddResponse, msgID)}
		}
		return nil
	}

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Add(NewAddRequest("cn=first,dc=example,dc=org", nil)))
	require.NoError(t, txn.Add(NewAddRequest("cn=second,dc=example,dc=org", nil)))

	err = txn.Commit()
	require.Error(t, err)

	var resultErr *TransactionResultError
	require.ErrorAs(t, err, &resultErr)
	assert.Equal(t, uint16(LDAPResultOperationsError), resultErr.ResultCode)
	assert.Equal(t, "transaction failed", resultErr.DiagnosticMessage)
	require.Len(t, resultErr.FailedOperations, 2)

	byMessageID := map[int64]TransactionOperationResult{}
	for _, failed := range resultErr.FailedOperations {
		byMessageID[failed.MessageID] = failed
	}

	first, ok := byMessageID[addMsgID]
	require.True(t, ok)
	assert.Equal(t, uint16(LDAPResultConstraintViolation), first.ResultCode)
	assert.Equal(t, "constraint", first.DiagnosticMessage)

	second, ok := byMessageID[secondAddMsgID]
	require.True(t, ok)
	assert.Equal(t, uint16(LDAPResultEntryAlreadyExists), second.ResultCode)

	// The reported message IDs resolve to the operations the caller staged.
	op := txn.Operation(addMsgID)
	require.NotNil(t, op)
	assert.Equal(t, "add", op.Kind)
	assert.Equal(t, "cn=first,dc=example,dc=org", op.DN)

	assert.NotNil(t, txn.Operation(secondAddMsgID))
	assert.Nil(t, txn.Operation(999999))
}

// TestTransactionCommitFailureMessageIDOnly covers the minimal server form in
// which each failed element contains only a messageID.
func TestTransactionCommitFailureMessageIDOnly(t *testing.T) {
	var addMsgID int64
	server := newTestTxnServer(t)
	conn := server.conn
	server.handler = func(req *ber.Packet) []*ber.Packet {
		msgID := server.requestMsgID(req)
		protocolOp := req.Children[1]
		switch protocolOp.Tag {
		case ber.Tag(ApplicationExtendedRequest):
			if addMsgID == 0 {
				return []*ber.Packet{testStartTxnResponse(msgID)}
			}
			outer := ber.Encode(ber.ClassContext, ber.TypePrimitive, 11, nil, "responseValue")
			seq := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Failed ops")
			element := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "Failed op")
			element.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive,
				ber.TagInteger, addMsgID, "messageID"))
			seq.AppendChild(element)
			outer.AppendChild(seq)
			return []*ber.Packet{testEndTxnResponse(msgID, LDAPResultOther, "nope", outer)}
		case ber.Tag(ApplicationAddRequest):
			addMsgID = msgID
			return []*ber.Packet{testUpdateResponse(ApplicationAddResponse, msgID)}
		}
		return nil
	}

	txn, err := conn.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Add(NewAddRequest("cn=first,dc=example,dc=org", nil)))

	err = txn.Commit()
	var resultErr *TransactionResultError
	require.ErrorAs(t, err, &resultErr)
	require.Len(t, resultErr.FailedOperations, 1)
	assert.Equal(t, addMsgID, resultErr.FailedOperations[0].MessageID)
	assert.Equal(t, uint16(0), resultErr.FailedOperations[0].ResultCode)
}

// TestTransactionStartFailure verifies a non-success Start Transaction
// response is surfaced as the LDAP error and no usable handle is returned.
func TestTransactionStartFailure(t *testing.T) {
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})

	go func() {
		req, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		msgID := packetMustMsgID(t, req)
		resp := testResponse(msgID,
			testLDAPResult(ApplicationExtendedResponse, LDAPResultUnwillingToPerform,
				"transactions disabled"), nil)
		_ = ptc.SendResponse(resp)
	}()

	txn, err := conn.StartTransaction()
	require.Error(t, err)
	assert.Nil(t, txn)
	assert.True(t, IsErrorWithCode(err, LDAPResultUnwillingToPerform))
}

// TestTransactionStartMissingValue verifies a successful Start response that
// omits the identifier is treated as a protocol error.
func TestTransactionStartMissingValue(t *testing.T) {
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})

	go func() {
		req, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		msgID := packetMustMsgID(t, req)
		_ = ptc.SendResponse(testResponse(msgID,
			testLDAPResult(ApplicationExtendedResponse, LDAPResultSuccess, ""), nil))
	}()

	txn, err := conn.StartTransaction()
	require.Error(t, err)
	assert.Nil(t, txn)
	assert.ErrorIs(t, err, ErrTransactionProtocolError)
}

// TestTransactionUpdateMissingControl verifies that an in-transaction update
// response that fails to echo the Transaction Specification Control is a
// protocol error and aborts the handle.
func TestTransactionUpdateMissingControl(t *testing.T) {
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})

	go func() {
		start, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = ptc.SendResponse(testStartTxnResponse(packetMustMsgID(t, start)))

		add, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		// Success but no transaction control echoed.
		_ = ptc.SendResponse(testResponse(packetMustMsgID(t, add),
			testLDAPResult(ApplicationAddResponse, LDAPResultSuccess, ""), nil))
	}()

	txn, err := conn.StartTransaction()
	require.NoError(t, err)

	err = txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTransactionProtocolError)
	assert.True(t, txn.Aborted())
}

// TestTransactionUpdateWrongIdentifier verifies that a response echoing a
// different transaction identifier is a protocol error.
func TestTransactionUpdateWrongIdentifier(t *testing.T) {
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})

	go func() {
		start, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = ptc.SendResponse(testStartTxnResponse(packetMustMsgID(t, start)))

		add, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		wrong := &ControlTransaction{Identifier: []byte("other-txn")}
		_ = ptc.SendResponse(testResponse(packetMustMsgID(t, add),
			testLDAPResult(ApplicationAddResponse, LDAPResultSuccess, ""),
			[]Control{wrong}))
	}()

	txn, err := conn.StartTransaction()
	require.NoError(t, err)

	err = txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTransactionProtocolError)
	assert.True(t, txn.Aborted())
}

// TestTransactionUpdateServerFailure verifies that an in-transaction update
// returning an LDAP error aborts the transaction and chains
// ErrTransactionAborted while preserving the LDAP result code.
func TestTransactionUpdateServerFailure(t *testing.T) {
	ptc := newPacketTranslatorConn()
	conn := NewConn(ptc, false)
	conn.Start()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = ptc.Close()
	})

	go func() {
		start, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = ptc.SendResponse(testStartTxnResponse(packetMustMsgID(t, start)))

		add, err := ptc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = ptc.SendResponse(testResponse(packetMustMsgID(t, add),
			testLDAPResult(ApplicationAddResponse, LDAPResultEntryAlreadyExists,
				"entry exists"), []Control{testTxnControl()}))
	}()

	txn, err := conn.StartTransaction()
	require.NoError(t, err)

	err = txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTransactionAborted)
	assert.True(t, IsErrorWithCode(err, LDAPResultEntryAlreadyExists))
	assert.True(t, txn.Aborted())

	err = txn.Commit()
	assert.ErrorIs(t, err, ErrTransactionAborted)
}

// TestTransactionBoundToOriginatingConnection verifies the handle is bound to
// the connection that started it: operations are only visible to that
// connection's server and never sent over a second connection.
func TestTransactionBoundToOriginatingConnection(t *testing.T) {
	firstPtc := newPacketTranslatorConn()
	first := NewConn(firstPtc, false)
	first.Start()

	secondPtc := newPacketTranslatorConn()
	second := NewConn(secondPtc, false)
	second.Start()

	t.Cleanup(func() {
		_ = first.Close()
		_ = firstPtc.Close()
		_ = second.Close()
		_ = secondPtc.Close()
	})

	// First connection's server answers Start and the update.
	go func() {
		req, err := firstPtc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = firstPtc.SendResponse(testStartTxnResponse(packetMustMsgID(t, req)))

		add, err := firstPtc.ReceiveRequest()
		if err != nil {
			return
		}
		_ = firstPtc.SendResponse(testUpdateResponse(ApplicationAddResponse,
			packetMustMsgID(t, add)))
	}()

	// The second connection's server must never see a transaction request.
	secondSawRequest := make(chan struct{}, 1)
	go func() {
		if _, err := secondPtc.ReceiveRequest(); err == nil {
			secondSawRequest <- struct{}{}
		}
	}()

	txn, err := first.StartTransaction()
	require.NoError(t, err)
	require.NoError(t, txn.Add(NewAddRequest("cn=alice,dc=example,dc=org", nil)))

	select {
	case <-secondSawRequest:
		t.Fatal("transaction operation leaked onto a different connection")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTransactionNilRequests(t *testing.T) {
	txn := &Transaction{}
	assert.Error(t, txn.Add(nil))
	assert.Error(t, txn.Modify(nil))
	assert.Error(t, txn.Del(nil))
	assert.Error(t, txn.ModifyDN(nil))
}

// TestControlTransactionRoundTrip checks the control encodes the bare
// identifier OCTET STRING and decodes back to the same value.
func TestControlTransactionRoundTrip(t *testing.T) {
	control := &ControlTransaction{Criticality: true, Identifier: []byte("abc\000123")}

	encoded := control.Encode()
	require.Len(t, encoded.Children, 3)

	decoded, err := DecodeControl(encoded)
	require.NoError(t, err)
	txn, ok := decoded.(*ControlTransaction)
	require.True(t, ok)
	assert.True(t, txn.Criticality)
	assert.Equal(t, []byte("abc\000123"), txn.Identifier)
	assert.Equal(t, ControlTypeTransaction, txn.GetControlType())

	// Criticality false omits the BOOLEAN.
	control.Criticality = false
	encoded = control.Encode()
	require.Len(t, encoded.Children, 2)
	decoded, err = DecodeControl(encoded)
	require.NoError(t, err)
	assert.False(t, decoded.(*ControlTransaction).Criticality)

	assert.Contains(t, control.String(), ControlTypeTransaction)
}
