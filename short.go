// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"encoding/binary"
	"io"
	"sync/atomic"
	"time"
)

const (
	defaultMaxShortMessageSize    uint32 = 1024
	defaultShortMessageBacklog    int    = 64
	maxNegotiatedShortMessageSize        = 1<<32 - 1

	sizeOfShortCapabilities = 4
	shortNegotiationTimeout = 30 * time.Second
)

type shortNegotiationState uint8

const (
	shortDisabled shortNegotiationState = iota
	shortNegotiating
	shortSupported
	shortReady
	shortUnsupported
)

type shortFrame struct {
	hdr  header
	body []byte
}

func (s *Session) initShortMessages() {
	if !s.config.EnableShortMessages {
		s.shortState = shortDisabled
		return
	}

	s.shortRecvCh = make(chan shortFrame, s.config.ShortMessageBacklog)
	s.shortSendCh = make(chan shortFrame, s.config.ShortMessageBacklog)
	s.shortDoneCh = make(chan struct{})
	s.shortState = shortNegotiating
	s.shortProbeID = atomic.AddUint32(&s.pingID, 1) - 1
}

func (s *Session) startShortMessageProbe() {
	if !s.config.EnableShortMessages {
		return
	}

	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN|flagShortMessages, 0, s.shortProbeID)
	if err := s.sendNoWait(hdr); err != nil {
		s.exitErr(err)
		return
	}
	s.startShortNegotiationTimer()
}

func (s *Session) startShortNegotiationTimer() {
	timeout := s.config.StreamOpenTimeout
	if timeout <= 0 {
		timeout = shortNegotiationTimeout
	}
	go func() {
		select {
		case <-s.shortDoneCh:
		case <-s.shutdownCh:
		case <-time.After(timeout):
			s.finishShortNegotiation(shortUnsupported, 0)
		}
	}()
}

func (s *Session) handleShortProbe() {
	if !s.config.EnableShortMessages {
		return
	}

	go func() {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typePing, flagACK|flagShortMessages, 0, s.shortProbeID)
		if err := s.sendNoWait(hdr); err != nil {
			return
		}
		s.startShortNegotiationTimer()
		go s.sendShortCapabilities()
	}()
}

func (s *Session) handleShortProbeAck() {
	if !s.config.EnableShortMessages {
		return
	}

	s.shortStateLock.Lock()
	if s.shortState == shortNegotiating {
		s.shortState = shortSupported
	}
	s.shortStateLock.Unlock()

	go s.sendShortCapabilities()
}

func (s *Session) sendShortCapabilities() {
	if !s.config.EnableShortMessages ||
		!atomic.CompareAndSwapInt32(&s.shortCapSent, 0, 1) {
		return
	}

	body := make([]byte, sizeOfShortCapabilities)
	binary.BigEndian.PutUint32(body[0:4], s.config.MaxShortMessageSize)
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeShortCapabilities, 0, 0, uint32(len(body)))
	if err := s.waitForSend(hdr, body); err != nil {
		atomic.CompareAndSwapInt32(&s.shortCapSent, 1, 0)
		s.exitErr(err)
	}
}

func (s *Session) handleShortCapabilities(hdr header) error {
	if !s.config.EnableShortMessages {
		return ErrInvalidMsgType
	}
	if hdr.StreamID() != 0 || hdr.Flags() != 0 || hdr.Length() != sizeOfShortCapabilities {
		return ErrInvalidMsgType
	}

	body := make([]byte, sizeOfShortCapabilities)
	if _, err := io.ReadFull(s.bufRead, body); err != nil {
		return err
	}

	maxMessageSize := binary.BigEndian.Uint32(body[0:4])
	if maxMessageSize == 0 {
		return ErrInvalidMsgType
	}
	s.finishShortNegotiation(shortReady, maxMessageSize)
	return nil
}

func (s *Session) finishShortNegotiation(state shortNegotiationState, maxMessageSize uint32) {
	s.shortStateLock.Lock()
	closeDone := false
	switch s.shortState {
	case shortReady, shortUnsupported:
		s.shortStateLock.Unlock()
		return
	case shortNegotiating, shortSupported:
		if state == shortReady {
			s.remoteShortMax = maxMessageSize
		}
		s.shortState = state
		if !s.shortClosed {
			s.shortClosed = true
			closeDone = true
		}
	}
	s.shortStateLock.Unlock()

	if closeDone {
		close(s.shortDoneCh)
	}
}

func (s *Session) waitShortMessages() (shortNegotiationState, error) {
	s.shortStateLock.Lock()
	state := s.shortState
	s.shortStateLock.Unlock()

	if state == shortDisabled {
		return shortUnsupported, ErrShortMessagesUnsupported
	}
	if state == shortReady || state == shortUnsupported {
		return state, nil
	}

	select {
	case <-s.shortDoneCh:
	case <-s.shutdownCh:
		return state, ErrSessionShutdown
	}

	s.shortStateLock.Lock()
	state = s.shortState
	s.shortStateLock.Unlock()
	return state, nil
}

// SendShortMessage sends one complete best-effort message. The returned count
// is the cumulative number of messages dropped from the local send queue.
func (s *Session) SendShortMessage(message []byte) (uint64, error) {
	state, err := s.waitShortMessages()
	if err != nil {
		return atomic.LoadUint64(&s.shortSendDrops), err
	}
	if state != shortReady {
		return atomic.LoadUint64(&s.shortSendDrops), ErrShortMessagesUnsupported
	}
	if uint64(len(message)) > uint64(s.remoteShortMax) {
		return atomic.LoadUint64(&s.shortSendDrops), ErrShortMessageTooLarge
	}

	body := make([]byte, len(message))
	copy(body, message)
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeShortMessage, 0, 0, uint32(len(body)))
	frame := shortFrame{hdr: hdr, body: body}

	s.shortSendLock.Lock()
	for len(s.shortSendCh) == cap(s.shortSendCh) {
		<-s.shortSendCh
		atomic.AddUint64(&s.shortSendDrops, 1)
	}
	s.shortSendCh <- frame
	dropped := atomic.LoadUint64(&s.shortSendDrops)
	s.shortSendLock.Unlock()

	return dropped, nil
}

// RecvShortMessage returns one complete message. dropped is the number of
// messages dropped from the local receive queue since the previous delivery.
func (s *Session) RecvShortMessage() ([]byte, uint64, error) {
	for {
		state, err := s.waitShortMessages()
		if err != nil {
			return nil, atomic.LoadUint64(&s.shortRecvDrops), err
		}
		if state != shortReady {
			return nil, atomic.LoadUint64(&s.shortRecvDrops), ErrShortMessagesUnsupported
		}

		select {
		case frame := <-s.shortRecvCh:
			dropped := atomic.SwapUint64(&s.shortRecvDrops, 0)
			return frame.body, dropped, nil
		case <-s.shortDoneCh:
			s.shortStateLock.Lock()
			state = s.shortState
			s.shortStateLock.Unlock()
			if state != shortReady {
				return nil, atomic.LoadUint64(&s.shortRecvDrops), ErrShortMessagesUnsupported
			}
		case <-s.shutdownCh:
			return nil, atomic.LoadUint64(&s.shortRecvDrops), ErrSessionShutdown
		}
	}
}

func (s *Session) handleShortMessage(hdr header) error {
	if !s.config.EnableShortMessages {
		return ErrInvalidMsgType
	}
	if hdr.StreamID() != 0 || hdr.Flags() != 0 {
		return ErrInvalidMsgType
	}
	length := hdr.Length()
	if length > s.config.MaxShortMessageSize {
		return ErrShortMessageTooLarge
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(s.bufRead, body); err != nil {
		return err
	}

	frame := shortFrame{
		hdr:  hdr,
		body: body,
	}
	select {
	case s.shortRecvCh <- frame:
	default:
		<-s.shortRecvCh
		atomic.AddUint64(&s.shortRecvDrops, 1)
		s.shortRecvCh <- frame
	}
	return nil
}
