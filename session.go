package milter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/textproto"
	"strings"
)

var errCloseSession = errors.New("Stop current milter processing")

// milterSession keeps session state during MTA communication
type milterSession struct {
	server        *Server
	actions       OptAction
	protocol      OptProtocol
	maxPacketSize int
	conn          net.Conn
	headers       textproto.MIMEHeader
	macros        map[string]string
	backend       Milter
}

// ReadPacket reads incoming milter packet
func (c *milterSession) ReadPacket() (*Message, error) {
	return readPacket(c.conn, 0, c.maxPacketSize)
}

// WritePacket sends a milter response packet to socket stream
func (m *milterSession) WritePacket(msg *Message) error {
	return writePacket(m.conn, msg, 0)
}

// Process processes incoming milter commands
func (m *milterSession) Process(msg *Message) (Response, error) {
	switch Code(msg.Code) {
	case CodeAbort:
		// abort current message and start over
		defer func() {
			m.headers = nil
			m.macros = nil
		}()
		return nil, m.backend.Abort(newModifier(m))

	case CodeBody:
		// body chunk
		return m.backend.BodyChunk(msg.Data, newModifier(m))

	case CodeConn:
		data := msg.Data
		// new connection, get hostname
		hostname := readCString(data)
		if len(data) < len(hostname)+1 {
			return RespTempFail, nil
		}
		data = data[len(hostname)+1:]
		// get protocol family
		if len(data) < 1 {
			return RespTempFail, nil
		}
		protocolFamily := data[0]
		data = data[1:]
		// get port
		var port uint16
		if protocolFamily == '4' || protocolFamily == '6' {
			if len(data) < 2 {
				return RespTempFail, nil
			}
			port = binary.BigEndian.Uint16(data)
			data = data[2:]
		}
		// get address
		address := readCString(data)
		// convert address and port to human readable string
		family := map[byte]string{
			'U': "unknown",
			'L': "unix",
			'4': "tcp4",
			'6': "tcp6",
		}
		// run handler and return
		return m.backend.Connect(
			hostname,
			family[protocolFamily],
			port,
			net.ParseIP(address),
			newModifier(m))

	case CodeMacro:
		// define macros
		m.macros = make(map[string]string)
		// convert data to Go strings
		var data []string
		if len(msg.Data) > 0 {
			data = decodeCStrings(msg.Data[1:])
		}
		if len(data) != 0 {
			if len(data)%2 == 1 {
				data = append(data, "")
			}

			// store data in a map
			for i := 0; i < len(data); i += 2 {
				m.macros[data[i]] = data[i+1]
			}
		}
		// do not send response
		return nil, nil

	case CodeEOB:
		// call and return milter handler
		return m.backend.Body(newModifier(m))

	case CodeHelo:
		// helo command
		name := strings.TrimSuffix(string(msg.Data), null)
		return m.backend.Helo(name, newModifier(m))

	case CodeHeader:
		// make sure headers is initialized
		if m.headers == nil {
			m.headers = make(textproto.MIMEHeader)
		}
		// add new header to headers map
		headerData := decodeCStrings(msg.Data)
		// headers with an empty body appear as `text\x00\x00`, decodeCStrings will drop the empty body
		if len(headerData) == 1 {
			headerData = append(headerData, "")
		}
		if len(headerData) == 2 {
			m.headers.Add(headerData[0], headerData[1])
			// call and return milter handler
			return m.backend.Header(headerData[0], headerData[1], newModifier(m))
		}

	case CodeMail:
		// envelope from address
		from := readCString(msg.Data)
		return m.backend.MailFrom(strings.Trim(from, "<>"), newModifier(m))

	case CodeEOH:
		// end of headers
		return m.backend.Headers(m.headers, newModifier(m))

	case CodeOptNeg:
		// ignore request and prepare response buffer
		var buffer bytes.Buffer
		// prepare response data
		for _, value := range []uint32{serverProtocolVersion, uint32(m.actions), uint32(m.protocol)} {
			if err := binary.Write(&buffer, binary.BigEndian, value); err != nil {
				return nil, err
			}
		}
		// append requested symlist
		if m.server.Actions&OptSetSymList != 0 {
			for stage, macros := range m.server.SymList {
				binary.Write(&buffer, binary.BigEndian, stage)
				for i, macro := range macros {
					if i != 0 {
						buffer.WriteByte(' ')
					}
					buffer.WriteString(macro)
				}
				buffer.WriteByte(0)
			}
		}
		// build and send packet
		return NewResponse('O', buffer.Bytes()), nil

	case CodeQuit:
		// client requested session close
		return nil, errCloseSession

	case CodeRcpt:
		// envelope to address
		to := readCString(msg.Data)
		return m.backend.RcptTo(strings.Trim(to, "<>"), newModifier(m))

	case CodeData:
		// data, ignore

	default:
		// print error and close session
		log.Printf("Unrecognized command code: %c", msg.Code)
		return nil, errCloseSession
	}

	// by default continue with next milter message
	return RespContinue, nil
}

// HandleMilterComands processes all milter commands in the same connection
func (m *milterSession) HandleMilterCommands() {
	defer m.conn.Close()

	// A malformed frame must never crash the server process.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic while handling milter command: %v", r)
		}
	}()

	for {
		msg, err := m.ReadPacket()
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading milter command: %v", err)
			}
			return
		}

		resp, err := m.Process(msg)
		if err != nil {
			if err != errCloseSession {
				// log error condition
				log.Printf("Error performing milter command: %v", err)
			}
			return
		}

		// ignore empty responses
		if resp != nil {
			// send back response message
			if err = m.WritePacket(resp.Response()); err != nil {
				log.Printf("Error writing packet: %v", err)
				return
			}

			if !resp.Continue() {
				// prepare backend for next message
				m.backend = m.server.NewMilter()
			}
		}
	}
}
