package milter

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
)

type recordedPacket struct {
	code byte
	data []byte
}

// newTestModifier builds a Modifier wired to a packet recorder.
func newTestModifier(actions OptAction, stage Stage) (*Modifier, *[]recordedPacket) {
	var packets []recordedPacket
	mod := &Modifier{
		actions: actions,
		stage:   stage,
		writePacket: func(msg *Message) error {
			data := make([]byte, len(msg.Data))
			copy(data, msg.Data)
			packets = append(packets, recordedPacket{code: msg.Code, data: data})
			return nil
		},
	}
	return mod, &packets
}

func TestModifier_RequiresNegotiatedCapability(t *testing.T) {
	cases := []struct {
		name    string
		actions OptAction
		call    func(*Modifier) error
		want    ModifyActCode
		missing OptAction
	}{
		{"AddHeader", OptChangeBody, func(m *Modifier) error { return m.AddHeader("H", "v") }, ActAddHeader, OptAddHeader},
		{"InsertHeader", OptChangeHeader, func(m *Modifier) error { return m.InsertHeader(1, "H", "v") }, ActInsertHeader, OptAddHeader},
		{"ChangeHeader", 0, func(m *Modifier) error { return m.ChangeHeader(1, "H", "v") }, ActChangeHeader, OptChangeHeader},
		{"ReplaceBody", 0, func(m *Modifier) error { return m.ReplaceBody([]byte("body")) }, ActReplBody, OptChangeBody},
		{"AddRecipient", 0, func(m *Modifier) error { return m.AddRecipient("a@x") }, ActAddRcpt, OptAddRcpt},
		{"DeleteRecipient", OptAddRcpt, func(m *Modifier) error { return m.DeleteRecipient("a@x") }, ActDelRcpt, OptRemoveRcpt},
		{"Quarantine", 0, func(m *Modifier) error { return m.Quarantine("spam") }, ActQuarantine, OptQuarantine},
		{"ChangeFrom", 0, func(m *Modifier) error { return m.ChangeFrom("s@x") }, ActChangeFrom, OptChangeFrom},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mod, packets := newTestModifier(tc.actions, StageEOM)

			if mod.ActionSupported(tc.want) {
				t.Fatalf("ActionSupported(%c) = true, want false", tc.want)
			}

			err := tc.call(mod)
			var nse *ActionNotSupportedError
			if !errors.As(err, &nse) {
				t.Fatalf("want *ActionNotSupportedError, got %T: %v", err, err)
			}
			if nse.Action != tc.want {
				t.Fatalf("error action = %c, want %c", nse.Action, tc.want)
			}
			if nse.Required != tc.missing {
				t.Fatalf("error required = %v, want %v", nse.Required, tc.missing)
			}
			if !strings.Contains(err.Error(), modifyActName(tc.want)) {
				t.Fatalf("error %q does not name action", err)
			}
			if !strings.Contains(err.Error(), tc.missing.String()) {
				t.Fatalf("error %q does not name missing capability %v", err, tc.missing)
			}
			if len(*packets) != 0 {
				t.Fatalf("failed action produced %d packets", len(*packets))
			}
		})
	}
}

func TestModifier_RequiresEOMStage(t *testing.T) {
	stages := []Stage{
		StageConnect,
		StageHelo,
		StageMailFrom,
		StageRcptTo,
		stageHeader,
		StageEOH,
		stageBody,
		stageAbort,
	}

	calls := []struct {
		name string
		code ModifyActCode
		call func(*Modifier) error
	}{
		{"AddHeader", ActAddHeader, func(m *Modifier) error { return m.AddHeader("H", "v") }},
		{"ChangeHeader", ActChangeHeader, func(m *Modifier) error { return m.ChangeHeader(1, "H", "v") }},
		{"InsertHeader", ActInsertHeader, func(m *Modifier) error { return m.InsertHeader(1, "H", "v") }},
		{"ReplaceBody", ActReplBody, func(m *Modifier) error { return m.ReplaceBody([]byte("x")) }},
		{"AddRecipient", ActAddRcpt, func(m *Modifier) error { return m.AddRecipient("a@x") }},
		{"DeleteRecipient", ActDelRcpt, func(m *Modifier) error { return m.DeleteRecipient("a@x") }},
		{"Quarantine", ActQuarantine, func(m *Modifier) error { return m.Quarantine("r") }},
		{"ChangeFrom", ActChangeFrom, func(m *Modifier) error { return m.ChangeFrom("s@x") }},
	}

	for _, stage := range stages {
		for _, tc := range calls {
			mod, packets := newTestModifier(OptAddHeader|OptChangeHeader|OptChangeBody|
				OptAddRcpt|OptRemoveRcpt|OptQuarantine|OptChangeFrom, stage)

			err := tc.call(mod)
			var nae *ActionNotAllowedError
			if !errors.As(err, &nae) {
				t.Fatalf("stage %d %s: want *ActionNotAllowedError, got %T: %v",
					stage, tc.name, err, err)
			}
			if nae.Action != tc.code {
				t.Fatalf("stage %d %s: error action = %c, want %c",
					stage, tc.name, nae.Action, tc.code)
			}
			if nae.Stage != stage {
				t.Fatalf("stage %d %s: error stage = %d", stage, tc.name, nae.Stage)
			}
			if len(*packets) != 0 {
				t.Fatalf("stage %d %s: produced %d packets", stage, tc.name, len(*packets))
			}
		}
	}
}

func TestModifier_ExpiresAfterCallback(t *testing.T) {
	mod, packets := newTestModifier(OptAddHeader|OptChangeFrom, StageEOM)
	mod.finish()

	if mod.ActionSupported(ActAddHeader) {
		t.Fatal("ActionSupported on expired modifier = true")
	}

	calls := []func() error{
		func() error { return mod.AddHeader("H", "v") },
		func() error { return mod.ChangeHeader(1, "H", "v") },
		func() error { return mod.InsertHeader(1, "H", "v") },
		func() error { return mod.ReplaceBody(nil) },
		func() error { return mod.AddRecipient("a@x") },
		func() error { return mod.DeleteRecipient("a@x") },
		func() error { return mod.Quarantine("r") },
		func() error { return mod.ChangeFrom("s@x") },
	}
	for i, call := range calls {
		if err := call(); !errors.Is(err, ErrExpired) {
			t.Fatalf("call %d on expired modifier: want ErrExpired, got %v", i, err)
		}
	}
	if len(*packets) != 0 {
		t.Fatalf("expired modifier produced %d packets", len(*packets))
	}
}

func TestModifier_WriteFailureInvalidates(t *testing.T) {
	var writes int
	mod := &Modifier{
		actions: OptAddHeader | OptQuarantine,
		stage:   StageEOM,
		writePacket: func(msg *Message) error {
			writes++
			return errors.New("connection reset")
		},
	}

	err := mod.AddHeader("H", "v")
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("want *WriteError, got %T: %v", err, err)
	}
	if we.Action != ActAddHeader {
		t.Fatalf("WriteError action = %c, want %c", we.Action, ActAddHeader)
	}
	if we.Unwrap() == nil || we.Unwrap().Error() != "connection reset" {
		t.Fatalf("WriteError does not wrap underlying error: %v", we.Unwrap())
	}

	if mod.ActionSupported(ActQuarantine) {
		t.Fatal("ActionSupported on invalidated modifier = true")
	}

	if err := mod.Quarantine("r"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("use after write failure: want ErrInvalid, got %v", err)
	}
	if writes != 1 {
		t.Fatalf("write called %d times after failure, want 1", writes)
	}
}

func TestModifier_ReplaceBodyChunks(t *testing.T) {
	mod, packets := newTestModifier(OptChangeBody, StageEOM)

	var body bytes.Buffer
	body.WriteString(strings.Repeat("a", MaxBodyChunk-1))
	body.WriteString("\r\n") // CR at end of chunk 1, LF at start of chunk 2
	body.WriteString("line2\r")
	body.WriteString("\n") // CRLF fully inside chunk 2
	body.WriteString(strings.Repeat("b", MaxBodyChunk-6))
	body.WriteString("\r\n") // straddles boundary again
	body.WriteString("tail")

	if err := mod.ReplaceBody(body.Bytes()); err != nil {
		t.Fatal(err)
	}

	if len(*packets) != 3 {
		t.Fatalf("got %d chunks, want 3", len(*packets))
	}
	for i, p := range *packets {
		if p.code != byte(ActReplBody) {
			t.Fatalf("chunk %d code = %c, want %c", i, p.code, ActReplBody)
		}
		if len(p.data) > MaxBodyChunk {
			t.Fatalf("chunk %d too big: %d", i, len(p.data))
		}
	}
	if len((*packets)[0].data) != MaxBodyChunk {
		t.Fatalf("first chunk = %d bytes, want %d", len((*packets)[0].data), MaxBodyChunk)
	}

	var reassembled bytes.Buffer
	for _, p := range *packets {
		reassembled.Write(p.data)
	}
	got := reassembled.Bytes()
	want := bytes.ReplaceAll(body.Bytes(), []byte{'\r', '\n'}, []byte{'\n'})
	if !bytes.Equal(got, want) {
		t.Fatalf("reassembled body mismatch: len got %d want %d", len(got), len(want))
	}
	if bytes.Contains(got, []byte{'\r'}) {
		t.Fatal("CR bytes remain after CRLF normalization")
	}
}

func TestModifier_ReplaceBodyMidwayFailure(t *testing.T) {
	var writes int
	mod := &Modifier{
		actions: OptChangeBody | OptAddHeader,
		stage:   StageEOM,
		writePacket: func(msg *Message) error {
			writes++
			if writes == 2 {
				return errors.New("broken pipe")
			}
			return nil
		},
	}

	body := bytes.Repeat([]byte("x"), 2*MaxBodyChunk+1)
	err := mod.ReplaceBody(body)
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("want *WriteError, got %T: %v", err, err)
	}
	if we.Action != ActReplBody {
		t.Fatalf("action = %c, want %c", we.Action, ActReplBody)
	}
	if writes != 2 {
		t.Fatalf("writes = %d, want 2", writes)
	}

	// After the failure no further action must be sent silently.
	err = mod.AddHeader("H", "v")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("AddHeader after failed ReplaceBody: want ErrInvalid, got %v", err)
	}
	err = mod.ReplaceBody([]byte("retry"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("retry ReplaceBody: want ErrInvalid, got %v", err)
	}
	if writes != 2 {
		t.Fatalf("packets sent after failure: writes = %d, want still 2", writes)
	}
}

func TestModifier_ReplaceBodyEmpty(t *testing.T) {
	mod, packets := newTestModifier(OptChangeBody, StageEOM)

	if err := mod.ReplaceBody(nil); err != nil {
		t.Fatal(err)
	}
	if len(*packets) != 1 {
		t.Fatalf("got %d packets for empty body, want 1", len(*packets))
	}
	if len((*packets)[0].data) != 0 {
		t.Fatalf("empty body chunk = %d bytes, want 0", len((*packets)[0].data))
	}
}

func TestModifier_HeaderPacketEncoding(t *testing.T) {
	mod, packets := newTestModifier(OptAddHeader|OptChangeHeader, StageEOM)

	if err := mod.AddHeader("X-Test", "a\r\nb"); err != nil {
		t.Fatal(err)
	}
	want := []byte("X-Test\x00a\nb\x00")
	if !bytes.Equal((*packets)[0].data, want) {
		t.Fatalf("AddHeader packet = %q, want %q", (*packets)[0].data, want)
	}

	if err := mod.ChangeHeader(2, "X-Test", "v"); err != nil {
		t.Fatal(err)
	}
	p := (*packets)[1]
	if p.code != byte(ActChangeHeader) {
		t.Fatalf("code = %c, want %c", p.code, ActChangeHeader)
	}
	if len(p.data) < 4 || p.data[0] != 0 || p.data[3] != 2 {
		t.Fatalf("missing 1-based big-endian index: % x", p.data[:4])
	}
}

// testSession starts an in-process milter server and client session.
func testSession(t *testing.T, backend Milter, serverActions, clientActions OptAction) *ClientSession {
	t.Helper()

	srv := Server{
		NewMilter: func() Milter { return backend },
		Actions:   serverActions,
	}
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(local)
	t.Cleanup(func() { srv.Close() })

	cl := NewClientWithOptions("tcp", local.Addr().String(), ClientOptions{
		ActionMask: clientActions,
	})
	t.Cleanup(func() { cl.Close() })

	session, err := cl.Session()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	return session
}

// eomMilter performs a fixed set of EOM modifications and records errors.
type eomMilter struct {
	NoOpMilter
	errs []error
}

func (mm *eomMilter) Body(m *Modifier) (Response, error) {
	if err := m.AddHeader("X-A", "1"); err != nil {
		mm.errs = append(mm.errs, err)
	}
	if err := m.ChangeFrom("new@example.org"); err != nil {
		mm.errs = append(mm.errs, err)
	}
	return RespAccept, nil
}

func TestModifier_NegotiatedSubset(t *testing.T) {
	mm := &eomMilter{}
	session := testSession(t, mm,
		OptAddHeader|OptChangeFrom|OptQuarantine,
		OptAddHeader,
	)

	modifyActs, act, err := session.End()
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActAccept {
		t.Fatalf("action = %c, want accept", act.Code)
	}

	if session.ActionOpts != OptAddHeader {
		t.Fatalf("negotiated actions = %v, want only %v", session.ActionOpts, OptAddHeader)
	}

	var nse *ActionNotSupportedError
	if len(mm.errs) != 1 || !errors.As(mm.errs[0], &nse) {
		t.Fatalf("want one ActionNotSupportedError for ChangeFrom, got %v", mm.errs)
	}
	if nse.Action != ActChangeFrom || nse.Required != OptChangeFrom {
		t.Fatalf("unexpected error detail: %+v", nse)
	}

	if len(modifyActs) != 1 || modifyActs[0].Code != ActAddHeader {
		t.Fatalf("modify acts = %+v, want only the AddHeader packet", modifyActs)
	}
}

// earlyStageMilter tries to modify during the MAIL FROM callback.
type earlyStageMilter struct {
	NoOpMilter
	err error
}

func (mm *earlyStageMilter) MailFrom(from string, m *Modifier) (Response, error) {
	mm.err = m.AddHeader("X-A", "1")
	return RespContinue, nil
}

func TestModifier_NotAllowedBeforeEOM(t *testing.T) {
	mm := &earlyStageMilter{}
	session := testSession(t, mm, OptAddHeader, OptAddHeader)

	act, err := session.Mail("from@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActContinue {
		t.Fatalf("MAIL action = %c, want continue", act.Code)
	}

	var nae *ActionNotAllowedError
	if !errors.As(mm.err, &nae) {
		t.Fatalf("want ActionNotAllowedError, got %v", mm.err)
	}
	if nae.Action != ActAddHeader || nae.Stage != StageMailFrom {
		t.Fatalf("unexpected error detail: %+v", nae)
	}

	modifyActs, act, err := session.End()
	if err != nil {
		t.Fatal(err)
	}
	if len(modifyActs) != 0 {
		t.Fatalf("no modification packets expected, got %+v", modifyActs)
	}
	if act.Code != ActAccept {
		t.Fatalf("EOB action = %c, want accept", act.Code)
	}
}

// staleModifierMilter saves the Modifier from the first EOM and reuses it for
// the next message.
type staleModifierMilter struct {
	NoOpMilter
	saved *Modifier
	errs  []error
}

func (mm *staleModifierMilter) Body(m *Modifier) (Response, error) {
	if mm.saved != nil {
		if err := mm.saved.AddHeader("X-Stale", "1"); err != nil {
			mm.errs = append(mm.errs, err)
		}
	}
	mm.saved = m
	return RespAccept, nil
}

func TestModifier_StaleModifierDelayedUse(t *testing.T) {
	mm := &staleModifierMilter{}
	session := testSession(t, mm, OptAddHeader, OptAddHeader)

	if _, _, err := session.End(); err != nil {
		t.Fatal(err)
	}

	act, err := session.Mail("from@example.org", nil)
	if err != nil {
		t.Fatal(err)
	}
	if act.Code != ActContinue {
		t.Fatalf("MAIL action = %c, want continue", act.Code)
	}

	modifyActs, _, err := session.End()
	if err != nil {
		t.Fatal(err)
	}
	if len(mm.errs) != 1 || !errors.Is(mm.errs[0], ErrExpired) {
		t.Fatalf("want ErrExpired from stale modifier, got %v", mm.errs)
	}
	if len(modifyActs) != 0 {
		t.Fatalf("stale modifier leaked packets: %+v", modifyActs)
	}
}

func TestOptAction_String(t *testing.T) {
	if got := OptAction(0).String(); got != "0" {
		t.Fatalf("zero action = %q, want 0", got)
	}
	got := (OptAddHeader | OptChangeBody).String()
	if !strings.Contains(got, "SMFIF_ADDHDRS") || !strings.Contains(got, "SMFIF_CHGBODY") {
		t.Fatalf("missing capability names in %q", got)
	}
	if got := OptAction(1 << 30).String(); !strings.Contains(got, "0x40000000") {
		t.Fatalf("unknown bit = %q", got)
	}
}
