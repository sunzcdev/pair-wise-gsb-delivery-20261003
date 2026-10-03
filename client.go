package milter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-message/textproto"
)

var ErrUnsupportedMilterVersion = fmt.Errorf("milter: negotiate: unsupported milter version")

var (
	// ErrResultNotCommitted is returned by ClientSession.EndWithCollector when
	// the milter reaches a non-success terminal state, sends an illegal
	// modification action, exceeds a collector budget or the connection breaks
	// before a success terminal was read. In all these cases the buffered
	// modification actions are not delivered to the caller.
	ErrResultNotCommitted = errors.New("milter: eob: result not committed")

	// ErrResultTooManyBytes is returned (wrapping ErrResultNotCommitted) when
	// the total wire size of buffered modification actions exceeds
	// ResultCollector.MaxBytes.
	ErrResultTooManyBytes = errors.New("milter: eob: modification actions exceed the total byte budget")

	// ErrResultTooManyActions is returned (wrapping ErrResultNotCommitted) when
	// the number of buffered modification actions exceeds
	// ResultCollector.MaxActions.
	ErrResultTooManyActions = errors.New("milter: eob: modification actions exceed the action count budget")
)

// Client is a wrapper for managing milter connections.
//
// Currently, it just creates new connections using provided Dialer.
type Client struct {
	opts    ClientOptions
	network string
	address string
}

type Dialer interface {
	Dial(network string, addr string) (net.Conn, error)
}

type ClientOptions struct {
	Dialer       Dialer
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	ActionMask   OptAction
	ProtocolMask OptProtocol
}

var defaultOptions = ClientOptions{
	Dialer: &net.Dialer{
		Timeout: 10 * time.Second,
	},
	ReadTimeout:  10 * time.Second,
	WriteTimeout: 10 * time.Second,
	ActionMask:   OptAddHeader | OptAddRcpt | OptChangeBody | OptChangeFrom | OptChangeHeader,
	ProtocolMask: 0,
}

// NewDefaultClient creates a new Client object using default options.
//
// It uses 10 seconds for connection/read/write timeouts and allows milter to
// send any actions supported by library.
func NewDefaultClient(network, address string) *Client {
	return NewClientWithOptions(network, address, defaultOptions)
}

// NewClientWithOptions creates a new Client object using provided options.
//
// You generally want to use options to restrict ActionMask to what your code
// supports and ProtocolMask to what you intend to submit.
//
// If opts.Dialer is not set, empty net.Dialer object will be used.
func NewClientWithOptions(network, address string, opts ClientOptions) *Client {
	if opts.Dialer == nil {
		opts.Dialer = &net.Dialer{}
	}

	return &Client{
		opts:    opts,
		network: network,
		address: address,
	}
}

func (c *Client) Session() (*ClientSession, error) {
	s := &ClientSession{
		readTimeout:           c.opts.ReadTimeout,
		writeTimeout:          c.opts.WriteTimeout,
		clientProtocolVersion: 6,
	}

	// TODO(foxcpp): Connection pooling.

	conn, err := c.opts.Dialer.Dial(c.network, c.address)
	if err != nil {
		return nil, fmt.Errorf("milter: session create: %w", err)
	}

	s.conn = conn
	if err := s.negotiate(c.opts.ActionMask, c.opts.ProtocolMask); err != nil {
		return nil, err
	}

	return s, nil
}

func (c *Client) Close() error {
	// Reserved for use in connection pooling.
	return nil
}

type ClientSession struct {
	conn net.Conn

	// Bitmask of negotiated action options.
	ActionOpts OptAction

	// Bitmask of negotiated protocol options.
	ProtocolOpts OptProtocol

	needAbort bool

	readTimeout  time.Duration
	writeTimeout time.Duration
	// Milter client version. Can be downgraded during negotiation
	clientProtocolVersion uint32
}

// negotiate exchanges OPTNEG messages with the milter and sets s.mask to the
// negotiated value.
func (s *ClientSession) negotiate(actionMask OptAction, protoMask OptProtocol) error {
	// Send our mask, get mask from milter..
	msg := &Message{
		Code: byte(CodeOptNeg), // TODO(foxcpp): Get rid of casts by changing msg.Code to have Code type
		Data: make([]byte, 4*3),
	}
	binary.BigEndian.PutUint32(msg.Data, s.clientProtocolVersion)
	binary.BigEndian.PutUint32(msg.Data[4:], uint32(actionMask))
	binary.BigEndian.PutUint32(msg.Data[8:], uint32(protoMask))

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return fmt.Errorf("milter: negotiate: optneg write: %w", err)
	}
	msg, err := readPacket(s.conn, s.readTimeout)
	if err != nil {
		return fmt.Errorf("milter: negotiate: optneg read: %w", err)
	}
	if Code(msg.Code) != CodeOptNeg {
		return fmt.Errorf("milter: negotiate: unexpected code: %v", rune(msg.Code))
	}
	if len(msg.Data) < 4*3 /* version + action mask + proto mask */ {
		return fmt.Errorf("milter: negotiate: unexpected data size: %v", len(msg.Data))
	}

	milterVersion := binary.BigEndian.Uint32(msg.Data[:4])
	milterActionMask := binary.BigEndian.Uint32(msg.Data[4:])
	s.ActionOpts = OptAction(milterActionMask)
	milterProtoMask := binary.BigEndian.Uint32(msg.Data[8:])
	s.ProtocolOpts = OptProtocol(milterProtoMask)

	// If milter advertises lower protocol version than we support, try to downgrade.
	if milterVersion < s.clientProtocolVersion {
		// Only downgrade if both sides support the same actions and protocols.
		// The lowest supported milterVersion is 2.
		if milterVersion >= 2 && actionMask&0x3f == actionMask && protoMask&0x7f == protoMask {
			s.clientProtocolVersion = milterVersion
		} else {
			return ErrUnsupportedMilterVersion
		}
	}

	s.needAbort = true

	return nil
}

// ProtocolOption checks whether the option is set in negotiated options, that
// is, requested by both sides.
func (s *ClientSession) ProtocolOption(opt OptProtocol) bool {
	return s.ProtocolOpts&opt != 0
}

// ActionOption checks whether the option is set in negotiated options, that
// is, requested by both sides.
func (s *ClientSession) ActionOption(opt OptAction) bool {
	return s.ActionOpts&opt != 0
}

func (s *ClientSession) Macros(code Code, kv ...string) error {
	// Note: kv is ...string with the expectation that the list of macro names
	// will be static and not dynamically constructed.

	msg := &Message{
		Code: byte(CodeMacro),
		Data: []byte{byte(code)},
	}
	for _, str := range kv {
		msg.Data = appendCString(msg.Data, str)
	}

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return fmt.Errorf("milter: macros: %w", err)
	}

	return nil
}

func appendUint16(dest []byte, val uint16) []byte {
	dest = append(dest, 0x00, 0x00)
	binary.BigEndian.PutUint16(dest[len(dest)-2:], val)
	return dest
}

type Action struct {
	Code ActionCode

	// SMTP code if Code == ActReplyCode.
	SMTPCode int
	// Reply text if Code == ActReplyCode.
	SMTPText string
}

func (s *ClientSession) readAction() (*Action, error) {
	for {
		msg, err := readPacket(s.conn, s.readTimeout)
		if err != nil {
			return nil, fmt.Errorf("action read: %w", err)
		}
		if msg.Code == 'p' /* progress */ {
			continue
		}
		if ActionCode(msg.Code) != ActContinue {
			s.needAbort = false
		}

		return parseAction(msg)
	}
}

func parseAction(msg *Message) (*Action, error) {
	act := &Action{
		Code: ActionCode(msg.Code),
	}
	var err error

	switch ActionCode(msg.Code) {
	case ActAccept, ActContinue, ActDiscard, ActReject, ActTempFail:
	case ActReplyCode:
		if len(msg.Data) <= 4 {
			return nil, fmt.Errorf("action read: unexpected data length: %v", len(msg.Data))
		}
		act.SMTPCode, err = strconv.Atoi(string(msg.Data[:3]))
		if err != nil {
			return nil, fmt.Errorf("action read: malformed SMTP code: %v", msg.Data[:3])
		}
		// There is 0x20 (' ') in between.
		act.SMTPText = readCString(msg.Data[4:])
	default:
		return nil, fmt.Errorf("action read: unexpected code: %v", msg.Code)
	}

	return act, nil
}

// Conn sends the connection information to the milter.
//
// It should be called once per milter session (from Session to Close).
func (s *ClientSession) Conn(hostname string, family ProtoFamily, port uint16, addr string) (*Action, error) {
	if s.ProtocolOpts&OptNoConnect != 0 {
		return &Action{Code: ActContinue}, nil
	}

	msg := &Message{
		Code: byte(CodeConn),
	}
	msg.Data = appendCString(msg.Data, hostname)
	msg.Data = append(msg.Data, byte(family))
	if family != FamilyUnknown {
		if family == FamilyInet || family == FamilyInet6 {
			msg.Data = appendUint16(msg.Data, port)
		}
		msg.Data = appendCString(msg.Data, addr)
	}

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: conn: %w", err)
	}

	if !s.ProtocolOption(OptNoConnReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: conn: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

// Helo sends the HELO hostname to the milter.
//
// It should be called once per milter session (from Session to Close).
func (s *ClientSession) Helo(helo string) (*Action, error) {
	// Synthesise response as if server replied "go on" while in fact it does
	// not support that message.
	if s.ProtocolOpts&OptNoHelo != 0 {
		return &Action{Code: ActContinue}, nil
	}

	msg := &Message{
		Code: byte(CodeHelo),
		Data: appendCString(nil, helo),
	}

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: helo: %w", err)
	}

	if !s.ProtocolOption(OptNoHeloReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: helo: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

func (s *ClientSession) Mail(sender string, esmtpArgs []string) (*Action, error) {
	if s.ProtocolOpts&OptNoMailFrom != 0 {
		return &Action{Code: ActContinue}, nil
	}

	msg := &Message{
		Code: byte(CodeMail),
	}

	msg.Data = appendCString(msg.Data, "<"+sender+">")
	for _, arg := range esmtpArgs {
		msg.Data = appendCString(msg.Data, arg)
	}

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: mail: %w", err)
	}

	if !s.ProtocolOption(OptNoMailReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: mail: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

func (s *ClientSession) Rcpt(rcpt string, esmtpArgs []string) (*Action, error) {
	if s.ProtocolOpts&OptNoRcptTo != 0 {
		return &Action{Code: ActContinue}, nil
	}

	msg := &Message{
		Code: byte(CodeRcpt),
	}

	msg.Data = appendCString(msg.Data, "<"+rcpt+">")
	for _, arg := range esmtpArgs {
		msg.Data = appendCString(msg.Data, arg)
	}

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: rcpt: %w", err)
	}

	if !s.ProtocolOption(OptNoRcptReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: rcpt: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

// HeaderField sends a single header field to the milter.
//
// Value should be the original field value without any unfolding applied.
//
// HeaderEnd() must be called after the last field.
func (s *ClientSession) HeaderField(key, value string) (*Action, error) {
	if s.ProtocolOpts&OptNoHeaders != 0 {
		return &Action{Code: ActContinue}, nil
	}

	msg := &Message{
		Code: byte(CodeHeader),
	}
	msg.Data = appendCString(msg.Data, key)
	msg.Data = appendCString(msg.Data, value)

	if err := writePacket(s.conn, msg, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: header field: %w", err)
	}

	if !s.ProtocolOption(OptNoHeaderReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: header field: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

// HeaderEnd send the EOH (End-Of-Header) message to the milter.
//
// No HeaderField calls are allowed after this point.
func (s *ClientSession) HeaderEnd() (*Action, error) {
	if s.ProtocolOpts&OptNoEOH != 0 {
		return &Action{Code: ActContinue}, nil
	}

	if err := writePacket(s.conn, &Message{
		Code: byte(CodeEOH),
	}, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: header end: %w", err)
	}

	if !s.ProtocolOption(OptNoEOHReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: header end: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

// Header sends each field from textproto.Header followed by EOH unless
// header messages are disabled during negotiation.
func (s *ClientSession) Header(hdr textproto.Header) (*Action, error) {
	for f := hdr.Fields(); f.Next(); {
		act, err := s.HeaderField(f.Key(), f.Value())
		if err != nil {
			return nil, err
		}

		if act.Code != ActContinue {
			return act, nil
		}
	}

	return s.HeaderEnd()
}

// BodyChunk sends a single body chunk to the milter.
//
// It is callers responsibility to ensure every chunk is not bigger than
// MaxBodyChunk.
//
// If OptSkip was specified during negotiation, caller should be ready to
// handle return ActSkip and stop sending body chunks if it is returned.
func (s *ClientSession) BodyChunk(chunk []byte) (*Action, error) {
	if s.ProtocolOpts&OptNoBody != 0 {
		return &Action{Code: ActContinue}, nil
	}

	// Callers tend to be irresponsible... /s
	if len(chunk) > MaxBodyChunk {
		return nil, fmt.Errorf("milter: body chunk: too big body chunk: %v", len(chunk))
	}

	if err := writePacket(s.conn, &Message{
		Code: byte(CodeBody),
		Data: chunk,
	}, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: body chunk: %w", err)
	}

	if !s.ProtocolOption(OptNoBodyReply) {
		act, err := s.readAction()
		if err != nil {
			return nil, fmt.Errorf("milter: body chunk: %w", err)
		}
		return act, nil
	}
	return &Action{Code: ActContinue}, nil
}

// BodyReadFrom is a helper function that calls BodyChunk repeately to transmit entire
// body from io.Reader and then calls End.
//
// See documentation for these functions for details.
func (s *ClientSession) BodyReadFrom(r io.Reader) ([]ModifyAction, *Action, error) {
	// It is problematic to use io.WriteCloser since we may need to report
	// action after each write.

	buf := make([]byte, MaxBodyChunk)
	for {
		n, err := r.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, nil, err
		}
		if n == 0 {
			break
		}

		act, err := s.BodyChunk(buf[:n])
		if err != nil {
			return nil, nil, err
		}
		if act.Code == ActSkip {
			break
		}
		if act.Code != ActContinue {
			return nil, act, nil
		}
	}

	return s.End()
}

type ModifyAction struct {
	Code ModifyActCode

	// Recipient to add/remove if Code == ActAddRcpt or ActDelRcpt.
	Rcpt string

	// New envelope sender if Code = ActChangeFrom.
	From string

	// ESMTP arguments for envelope sender if Code = ActChangeFrom.
	FromArgs []string

	// Portion of body to be replaced if Code == ActReplBody.
	Body []byte

	// Index of the header field to be changed if Code = ActChangeHeader or Code = ActInsertHeader.
	// Index is 1-based and is per value of HdrName.
	// E.g. HeaderIndex = 3 and HdrName = "DKIM-Signature" mean "change third
	// DKIM-Signature field". Order is the same as of HeaderField calls.
	HeaderIndex uint32

	// Header field name to be added/changed if Code == ActAddHeader or
	// ActChangeHeader or ActInsertHeader.
	HeaderName string

	// Header field value to be added/changed if Code == ActAddHeader or
	// ActChangeHeader or ActInsertHeader. If set to empty string - the field
	// should be removed.
	HeaderValue string

	// Quarantine reason if Code == ActQuarantine.
	Reason string
}

func parseModifyAct(msg *Message) (*ModifyAction, error) {
	act := &ModifyAction{
		Code: ModifyActCode(msg.Code),
	}

	switch ModifyActCode(msg.Code) {
	case ActAddRcpt, ActDelRcpt:
		act.Rcpt = readCString(msg.Data)
	case ActQuarantine:
		act.Reason = readCString(msg.Data)
	case ActReplBody:
		act.Body = msg.Data
	case ActChangeFrom:
		argv := bytes.Split(msg.Data, []byte{0x00})
		act.From = string(argv[0])
		for _, arg := range argv[1:] {
			act.FromArgs = append(act.FromArgs, string(arg))
		}
	case ActChangeHeader, ActInsertHeader:
		if len(msg.Data) < 4 {
			return nil, fmt.Errorf("read modify action: missing header index")
		}
		act.HeaderIndex = binary.BigEndian.Uint32(msg.Data)

		msg.Data = msg.Data[4:]
		fallthrough
	case ActAddHeader:
		// TODO: Change readCString to return last index.
		act.HeaderName = readCString(msg.Data)
		nul := bytes.IndexByte(msg.Data, 0x00)
		if nul == -1 {
			return nil, fmt.Errorf("read modify action: missing NUL delimiter")
		}
		if nul == len(msg.Data) {
			return nil, fmt.Errorf("read modify action: missing header value")
		}
		act.HeaderValue = readCString(msg.Data[nul+1:])
	default:
		return nil, fmt.Errorf("read modify action: unexpected message code: %v", msg.Code)
	}

	return act, nil
}

// readEOBResponse reads the sequence of messages sent in response to EOB.
//
// Every modification action is handed to onModify as it arrives. The wireSize
// argument is the size of the action message on the wire, including the single
// command byte. msg is the raw message and act the parsed modification action
// (msg.Data for header actions is resliced by the parser, see parseModifyAct).
// 'p' (progress) messages are consumed silently. Reading stops at the first
// accept/reject action, which is returned.
//
// If onModify returns an error, reading stops and the error is returned.
func (s *ClientSession) readEOBResponse(onModify func(msg *Message, act ModifyAction, wireSize int) error) (*Action, error) {
	for {
		msg, err := readPacket(s.conn, s.readTimeout)
		if err != nil {
			return nil, fmt.Errorf("action read: %w", err)
		}
		if msg.Code == 'p' /* progress */ {
			continue
		}

		if !isModifyActCode(msg.Code) {
			// Terminal accept/reject action.
			if ActionCode(msg.Code) != ActContinue {
				s.needAbort = false
			}
			return parseAction(msg)
		}

		wireSize := 1 + len(msg.Data)
		modifyAct, err := parseModifyAct(msg)
		if err != nil {
			return nil, err
		}
		if err := onModify(msg, *modifyAct, wireSize); err != nil {
			return nil, err
		}
	}
}

func isModifyActCode(code byte) bool {
	switch ModifyActCode(code) {
	case ActAddRcpt, ActDelRcpt, ActReplBody, ActChangeHeader, ActInsertHeader,
		ActAddHeader, ActChangeFrom, ActQuarantine:
		return true
	}
	return false
}

func (s *ClientSession) readModifyActs() (modifyActs []ModifyAction, act *Action, err error) {
	// Streaming mode with an internal accumulator: no budgets or legality
	// checks are applied and actions are collected independently of the final
	// verdict, see ClientSession.End.
	act, err = s.readEOBResponse(func(_ *Message, modifyAct ModifyAction, _ int) error {
		modifyActs = append(modifyActs, modifyAct)
		return nil
	})
	return modifyActs, act, err
}

// End sends the EOB message and resets session back to the state before Mail
// call. The same ClientSession can be used to check another message arrived
// within the same SMTP connection (Helo and Conn information is preserved).
//
// End operates in streaming mode: every modification action sent by the
// milter in response to EOB is parsed and returned as soon as it is read,
// regardless of the final accept/reject verdict. There is no byte or action
// count limit and no protocol-level legality validation beyond what is
// required to parse a message. Consequently the caller can observe actions
// for a message that is ultimately rejected or temp-failed; the returned
// slice must not be applied unless the terminal Action indicates success.
// The caller owns the returned slice; action payloads (e.g. ModifyAction.Body)
// reference buffers owned by the caller once returned.
//
// Use EndStream to handle each action through a callback as it arrives, or
// EndWithCollector to buffer actions and receive them only after a success
// terminal verdict.
//
// Close should be called to conclude session.
func (s *ClientSession) End() ([]ModifyAction, *Action, error) {
	if err := writePacket(s.conn, &Message{
		Code: byte(CodeEOB),
	}, s.writeTimeout); err != nil {
		return nil, nil, fmt.Errorf("milter: end: %w", err)
	}

	modifyActs, act, err := s.readModifyActs()
	if err != nil {
		return nil, nil, fmt.Errorf("milter: end: %w", err)
	}

	return modifyActs, act, nil
}

// EndStream sends the EOB message and invokes onModify for each modification
// action as it is received, preserving wire order, before returning the
// terminal accept/reject action. It behaves like End (streaming mode): there
// are no budgets, parsed actions are not additionally validated and onModify
// is called for all actions even if the final verdict is a rejection. The
// effect of an action is whatever onModify does with it immediately; the
// client library keeps no copy. If onModify returns an error, reading stops
// and the error is returned without reading the terminal verdict; in that
// case the session must be concluded with Abort or Close.
//
// A nil onModify means no modification actions are delivered (they are still
// consumed from the connection).
func (s *ClientSession) EndStream(onModify func(act ModifyAction) error) (*Action, error) {
	if err := writePacket(s.conn, &Message{
		Code: byte(CodeEOB),
	}, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: end: %w", err)
	}

	act, err := s.readEOBResponse(func(_ *Message, modifyAct ModifyAction, _ int) error {
		if onModify == nil {
			return nil
		}
		return onModify(modifyAct)
	})
	if err != nil {
		return nil, fmt.Errorf("milter: end: %w", err)
	}
	return act, nil
}

// ResultCollector buffers modification actions received in response to EOB
// until the milter reaches a success terminal verdict.
//
// The collector is purely in-memory: it is not backed by a database or disk
// and does not perform any network I/O of its own. It is not safe for
// concurrent use; use a separate collector per message/session.
type ResultCollector struct {
	// MaxBytes bounds the total on-the-wire size (in bytes) of buffered
	// modification actions, counting each command byte and payload. Zero
	// means no byte budget.
	MaxBytes int
	// MaxActions bounds the number of buffered modification actions. Zero
	// means no action count budget.
	MaxActions int

	// Actions holds the buffered modification actions in the order received.
	// It is populated by EndWithCollector only when the milter reaches a
	// success terminal verdict; otherwise it is left empty.
	Actions []ModifyAction

	totalBytes int
}

// TotalBytes returns the accumulated on-the-wire size of the buffered
// modification actions.
func (c *ResultCollector) TotalBytes() int {
	return c.totalBytes
}

func (c *ResultCollector) reset() {
	c.Actions = nil
	c.totalBytes = 0
}

// EndWithCollector sends the EOB message and buffers modification actions in
// c without delivering them to the caller until the milter reaches a success
// terminal verdict (ActAccept, ActDiscard or ActContinue at EOB). Actions are
// buffered in-memory only, in the order received.
//
// The buffered result is not delivered (c.Actions stays nil and an error
// wrapping ErrResultNotCommitted is returned together with the terminal
// action, when available) if:
//   - the terminal verdict is ActReject, ActTempFail or ActReplyCode;
//   - a modification action violates the milter protocol (illegal action);
//   - the cumulative wire size exceeds c.MaxBytes (ErrResultTooManyBytes);
//   - the number of actions exceeds c.MaxActions (ErrResultTooManyActions);
//   - the connection breaks before the terminal verdict is read.
//
// On any such outcome the caller must treat the message as unchanged and
// conclude the session with Abort or Close. On success c.Actions contains all
// actions in wire order and can be applied as a single atomic batch; the
// caller owns c.Actions.
//
// Unlike the streaming End/EndStream modes, actions never become visible to
// the caller before the success verdict, so a later rejection or a dropped
// connection cannot leave partially applied modifications.
func (s *ClientSession) EndWithCollector(c *ResultCollector) (*Action, error) {
	if err := writePacket(s.conn, &Message{
		Code: byte(CodeEOB),
	}, s.writeTimeout); err != nil {
		return nil, fmt.Errorf("milter: end: %w", err)
	}

	c.reset()

	act, err := s.readEOBResponse(func(msg *Message, modifyAct ModifyAction, wireSize int) error {
		if err := validateModifyAct(msg, modifyAct); err != nil {
			return fmt.Errorf("%w: %v", ErrResultNotCommitted, err)
		}

		c.totalBytes += wireSize
		if c.MaxBytes > 0 && c.totalBytes > c.MaxBytes {
			return fmt.Errorf("%w: %w", ErrResultNotCommitted, ErrResultTooManyBytes)
		}
		if c.MaxActions > 0 && len(c.Actions)+1 > c.MaxActions {
			return fmt.Errorf("%w: %w", ErrResultNotCommitted, ErrResultTooManyActions)
		}

		c.Actions = append(c.Actions, modifyAct)
		return nil
	})
	if err != nil {
		// Connection break, malformed/illegal action or budget overflow: no
		// terminal verdict was read and nothing is committed. Callback errors
		// already wrap ErrResultNotCommitted; other read/parse errors do not.
		c.reset()
		if errors.Is(err, ErrResultNotCommitted) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrResultNotCommitted, err)
	}

	if !isSuccessTerminal(act.Code) {
		c.reset()
		return act, fmt.Errorf("%w: terminal verdict %v", ErrResultNotCommitted, act.Code)
	}

	return act, nil
}

// isSuccessTerminal reports whether the accept/reject action represents a
// success terminal state at EOB. ActContinue functions the same as ActAccept
// at the end of the conversation; ActDiscard accepts the message (it is
// accepted but not delivered). ActReject, ActTempFail and ActReplyCode are
// non-success terminals.
func isSuccessTerminal(code ActionCode) bool {
	switch code {
	case ActAccept, ActDiscard, ActContinue:
		return true
	}
	return false
}

// validateModifyAct checks a modification action for milter protocol-level
// legality in addition to what is needed to parse it. It does not enforce the
// negotiated action mask: it is the milter's responsibility not to send
// actions the MTA did not advertise.
func validateModifyAct(msg *Message, act ModifyAction) error {
	switch act.Code {
	case ActAddRcpt, ActDelRcpt, ActQuarantine, ActChangeFrom:
		// String fields are NUL-terminated on the wire.
		if len(msg.Data) == 0 || msg.Data[len(msg.Data)-1] != 0x00 {
			return fmt.Errorf("read modify action: %v: value is not NUL-terminated", act.Code)
		}
	}

	switch act.Code {
	case ActAddRcpt, ActDelRcpt:
		if act.Rcpt == "" {
			return fmt.Errorf("read modify action: empty recipient")
		}
	case ActChangeHeader:
		// Per the milter protocol the index is 1-based and per header name:
		// it refers to the occurrence of a field with the supplied name, in
		// the order the fields were transmitted. Zero is invalid.
		if act.HeaderIndex == 0 {
			return fmt.Errorf("read modify action: change header index must be 1-based, got 0")
		}
		if act.HeaderName == "" {
			return fmt.Errorf("read modify action: empty header name")
		}
	case ActInsertHeader:
		// InsertHeader uses a 1-based index into the full header list.
		if act.HeaderIndex == 0 {
			return fmt.Errorf("read modify action: insert header index must be 1-based, got 0")
		}
		if act.HeaderName == "" {
			return fmt.Errorf("read modify action: empty header name")
		}
	case ActAddHeader:
		if act.HeaderName == "" {
			return fmt.Errorf("read modify action: empty header name")
		}
	case ActChangeFrom:
		if act.From == "" {
			return fmt.Errorf("read modify action: empty envelope sender")
		}
	case ActReplBody, ActQuarantine:
		// An empty replacement body and an empty reason are allowed.
	}
	return nil
}

// Abort sends Abort to the milter.
//
// This is called for an unexpected end to an email outside the milters
// control.
func (s *ClientSession) Abort() error {
	return writePacket(s.conn, &Message{
		Code: byte(CodeAbort),
	}, s.writeTimeout)
}

// Close releases resources associated with the session.
//
// If there a milter sequence in progress - it is aborted.
func (s *ClientSession) Close() error {
	if s.needAbort {
		_ = s.Abort()
	}

	if err := writePacket(s.conn, &Message{
		Code: byte(CodeQuit),
	}, s.writeTimeout); err != nil {
		return fmt.Errorf("milter: close: %w", err)
	}
	return s.conn.Close()
}
