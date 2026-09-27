// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package yamux

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// StreamMetadata is the bounded set of attributes an opener may attach to
// a stream when the service metadata extension has been negotiated. It
// lets the receiving side route or reject the stream before handing it
// to the application via Accept.
type StreamMetadata struct {
	// Service names the logical service the stream targets.
	Service string

	// Attributes carries optional key/value pairs, e.g. version or
	// tenant information.
	Attributes map[string]string
}

// AdmissionHandler is invoked on the receiving side once the service
// metadata for a new stream has arrived. Returning a nil error admits
// the stream: it is queued for Accept and acknowledged. Returning a
// non-nil error rejects the stream: it is reset and never surfaced to
// the application.
//
// The handler runs in its own goroutine per stream, so a slow handler
// never blocks data or control frames of other streams. The context is
// canceled when the session shuts down or the stream is reset while the
// handler is still running; handlers should honor it.
type AdmissionHandler func(ctx context.Context, meta *StreamMetadata) error

// encodeMetadata serializes the metadata into a bounded binary payload.
func encodeMetadata(meta *StreamMetadata) ([]byte, error) {
	size := 2 + len(meta.Service) + 2
	for k, v := range meta.Attributes {
		size += 2 + len(k) + 2 + len(v)
	}
	if size > int(hardMaxStreamMetadataSize) {
		return nil, ErrMetadataTooLarge
	}
	if len(meta.Service) > 0xFFFF || len(meta.Attributes) > 0xFFFF {
		return nil, ErrMetadataTooLarge
	}

	buf := make([]byte, 0, size)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(meta.Service)))
	buf = append(buf, meta.Service...)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(meta.Attributes)))
	for k, v := range meta.Attributes {
		if len(k) > 0xFFFF || len(v) > 0xFFFF {
			return nil, ErrMetadataTooLarge
		}
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(k)))
		buf = append(buf, k...)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(v)))
		buf = append(buf, v...)
	}
	return buf, nil
}

// decodeMetadata parses a metadata payload with strict bounds checking.
func decodeMetadata(buf []byte) (*StreamMetadata, error) {
	meta := &StreamMetadata{}
	read := func(n int) ([]byte, error) {
		if n < 0 || len(buf) < n {
			return nil, ErrInvalidMetadata
		}
		b := buf[:n]
		buf = buf[n:]
		return b, nil
	}
	readLen := func() (int, error) {
		b, err := read(2)
		if err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint16(b)), nil
	}

	n, err := readLen()
	if err != nil {
		return nil, err
	}
	svc, err := read(n)
	if err != nil {
		return nil, err
	}
	meta.Service = string(svc)

	count, err := readLen()
	if err != nil {
		return nil, err
	}
	if count > 0 {
		meta.Attributes = make(map[string]string, count)
	}
	for i := 0; i < count; i++ {
		klen, err := readLen()
		if err != nil {
			return nil, err
		}
		k, err := read(klen)
		if err != nil {
			return nil, err
		}
		vlen, err := readLen()
		if err != nil {
			return nil, err
		}
		v, err := read(vlen)
		if err != nil {
			return nil, err
		}
		meta.Attributes[string(k)] = string(v)
	}
	if len(buf) != 0 {
		return nil, ErrInvalidMetadata
	}
	return meta, nil
}

// serviceMetaEnabled reports whether the local endpoint participates in
// the service metadata extension.
func (s *Session) serviceMetaEnabled() bool {
	return s.config.EnableServiceMetadata || s.config.AdmissionHandler != nil
}

// serviceMetaActive reports whether the extension is enabled locally and
// was successfully negotiated with the peer.
func (s *Session) serviceMetaActive() bool {
	return s.serviceMetaEnabled() &&
		atomic.LoadUint32(&s.peerExtCaps)&extCapServiceMeta != 0
}

// maxMetadataSize returns the configured bound on metadata payloads.
func (s *Session) maxMetadataSize() uint32 {
	if s.config.MaxStreamMetadataSize == 0 {
		return defaultMaxStreamMetadataSize
	}
	return min(s.config.MaxStreamMetadataSize, hardMaxStreamMetadataSize)
}

// negotiateServiceMeta performs capability negotiation using ordinary
// Ping frames, which unaware peers simply echo back. It never emits a
// frame type an old peer would reject.
func (s *Session) negotiateServiceMeta() {
	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, extPingQuery)
	if err := s.waitForSend(hdr, nil); err != nil {
		return
	}
	// The response (or a peer-initiated query) completes the negotiation
	// via completeServiceMetaNegotiation. Nothing else to do here; if the
	// session dies first, waiters are unblocked by shutdownCh.
}

// completeServiceMetaNegotiation records the peer's capabilities and
// unblocks any OpenService callers. It is idempotent.
func (s *Session) completeServiceMetaNegotiation(caps uint32) {
	atomic.StoreUint32(&s.peerExtCaps, caps)
	s.extOnce.Do(func() { close(s.extDoneCh) })
}

// awaitServiceMetaNegotiation blocks until negotiation concludes.
func (s *Session) awaitServiceMetaNegotiation(ctx context.Context) error {
	if !s.serviceMetaEnabled() {
		return ErrServiceMetaUnsupported
	}
	select {
	case <-s.extDoneCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
	if atomic.LoadUint32(&s.peerExtCaps)&extCapServiceMeta == 0 {
		return ErrServiceMetaUnsupported
	}
	return nil
}

// OpenService opens a stream carrying bounded service metadata and waits
// until the peer admits or rejects it. The peer must have negotiated the
// service metadata extension, otherwise ErrServiceMetaUnsupported is
// returned and no extension frames are emitted. A rejected stream
// returns ErrStreamRejected.
func (s *Session) OpenService(ctx context.Context, service string, attrs map[string]string) (*Stream, error) {
	if err := s.awaitServiceMetaNegotiation(ctx); err != nil {
		return nil, err
	}

	payload, err := encodeMetadata(&StreamMetadata{Service: service, Attributes: attrs})
	if err != nil {
		return nil, err
	}
	if uint32(len(payload)) > s.maxMetadataSize() {
		return nil, fmt.Errorf("%w: %d bytes exceeds limit %d", ErrMetadataTooLarge, len(payload), s.maxMetadataSize())
	}

	stream, err := s.openStream(flagMETA)
	if err != nil {
		return nil, err
	}

	hdr := header(make([]byte, headerSize))
	hdr.encode(typeStreamMeta, 0, stream.id, uint32(len(payload)))
	if err := s.waitForSend(hdr, payload); err != nil {
		_ = stream.Close()
		return nil, err
	}

	// Wait for the peer to ACK (admitted) or RST (rejected) the stream.
	select {
	case <-stream.establishCh:
	case <-ctx.Done():
		_ = stream.Close()
		return nil, ctx.Err()
	case <-s.shutdownCh:
		return nil, ErrSessionShutdown
	}

	stream.stateLock.Lock()
	state := stream.state
	stream.stateLock.Unlock()
	switch state {
	case streamEstablished:
		return stream, nil
	case streamReset:
		return nil, ErrStreamRejected
	default:
		return nil, ErrStreamClosed
	}
}

// handleStreamMeta processes an incoming StreamMeta frame. The payload
// is always drained to keep the frame stream parseable, and admission
// runs in a separate goroutine so a slow handler cannot stall the
// receive loop for other streams.
func (s *Session) handleStreamMeta(hdr header) error {
	id := hdr.StreamID()
	length := hdr.Length()

	if !s.serviceMetaActive() {
		s.logger.Printf("[ERR] yamux: received stream metadata without negotiation")
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrInvalidMsgType
	}
	if length > hardMaxStreamMetadataSize {
		s.logger.Printf("[ERR] yamux: stream metadata exceeds hard limit (stream: %d, size: %d)", id, length)
		if sendErr := s.sendNoWait(s.goAway(goAwayProtoErr)); sendErr != nil {
			s.logger.Printf("[WARN] yamux: failed to send go away: %v", sendErr)
		}
		return ErrMetadataTooLarge
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(s.bufRead, payload); err != nil {
		s.logger.Printf("[ERR] yamux: failed to read stream metadata: %v", err)
		return err
	}

	s.streamLock.Lock()
	stream := s.streams[id]
	s.streamLock.Unlock()

	if stream == nil {
		s.logger.Printf("[WARN] yamux: discarding metadata for stream: %d", id)
		return nil
	}

	// Unknown or already handled stream: drop the metadata, the stream
	// was likely reset in the meantime.
	stream.stateLock.Lock()
	awaiting := stream.awaitMeta
	if awaiting {
		stream.awaitMeta = false
	}
	stream.stateLock.Unlock()
	if !awaiting {
		s.logger.Printf("[WARN] yamux: discarding unexpected metadata for stream: %d", id)
		return nil
	}
	if stream.metaTimer != nil {
		stream.metaTimer.Stop()
	}

	reject := func() {
		s.rejectStream(stream)
	}

	if length > s.maxMetadataSize() {
		s.logger.Printf("[WARN] yamux: stream metadata exceeds configured limit (stream: %d, size: %d)", id, length)
		reject()
		return nil
	}

	meta, err := decodeMetadata(payload)
	if err != nil {
		s.logger.Printf("[WARN] yamux: failed to decode stream metadata (stream: %d): %v", id, err)
		reject()
		return nil
	}

	stream.meta = meta

	if s.config.AdmissionHandler == nil {
		// No admission control configured: admit directly.
		s.admitStream(stream)
		return nil
	}
	go s.runAdmission(stream, meta)
	return nil
}

// runAdmission executes the admission handler for a pending stream. It
// runs in its own goroutine so slow or cancellable handlers never block
// the receive loop or other streams.
func (s *Session) runAdmission(stream *Stream, meta *StreamMetadata) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the handler context if the session dies or the stream is
	// closed/reset while admission is still pending.
	go func() {
		select {
		case <-s.shutdownCh:
			cancel()
		case <-stream.establishCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := s.config.AdmissionHandler(ctx, meta); err != nil {
		s.logger.Printf("[INFO] yamux: stream %d rejected by admission handler: %v", stream.id, err)
		s.rejectStream(stream)
		return
	}
	s.admitStream(stream)
}

// admitStream queues an admitted stream for Accept, applying the same
// backlog backpressure as incomingStream.
func (s *Session) admitStream(stream *Stream) {
	select {
	case s.acceptCh <- stream:
	default:
		s.logger.Printf("[WARN] yamux: backlog exceeded, forcing connection reset")
		s.rejectStream(stream)
	}
}

// rejectStream resets a stream that failed admission control.
func (s *Session) rejectStream(stream *Stream) {
	stream.stateLock.Lock()
	state := stream.state
	stream.stateLock.Unlock()
	if state == streamReset || state == streamClosed {
		return
	}

	stream.forceClose()
	s.closeStream(stream.id)

	hdr := header(make([]byte, headerSize))
	hdr.encode(typeWindowUpdate, flagRST, stream.id, 0)
	if err := s.sendNoWait(hdr); err != nil {
		s.logger.Printf("[WARN] yamux: failed to send stream reset: %v", err)
	}
}

// metaWaitTimeout bounds how long a stream may wait for its announced
// StreamMeta frame before being reset.
func (s *Session) metaWaitTimeout() time.Duration {
	if s.config.StreamOpenTimeout > 0 {
		return s.config.StreamOpenTimeout
	}
	return 75 * time.Second
}
