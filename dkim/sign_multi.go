package dkim

import (
	"bufio"
	"fmt"
	"hash"
	"io"
)

// SignResult is the outcome of a single signature produced by a MultiSigner.
type SignResult struct {
	// Signature is the whole DKIM-Signature header field. It contains the
	// header field name, its value and the final CRLF. The caller should
	// prepend it to the message.
	//
	// It is empty if Err is non-nil.
	Signature string
	// Err is set when this particular signature could not be generated, e.g.
	// because the crypto.Signer failed. Other signatures produced by the same
	// MultiSigner are unaffected.
	Err error
}

// MultiSigner generates multiple DKIM signatures for a single message, e.g.
// to sign with both an old and a new key during key rotation.
//
// The whole message header and body must be written to the MultiSigner. The
// message is streamed: it is read only once and is not buffered in memory, so
// memory usage doesn't grow with the message size. Signatures sharing the
// same body canonicalization algorithm share the canonicalization work.
//
// Close should always be called (either after the whole message has been
// written, or after an error occurred and the multi-signer won't be used
// anymore). Close waits for the internal pipeline to terminate and returns an
// error if the message itself could not be processed. Failures of individual
// signatures are reported via Results instead.
//
// After a successful Close, Results can be called to retrieve one SignResult
// per SignOptions value.
type MultiSigner struct {
	pw      *io.PipeWriter
	done    <-chan struct{}
	err     error        // only valid after done is closed
	results []SignResult // only valid after done is closed
}

// NewMultiSigner creates a new multi-signer. It returns an error if any of
// the SignOptions values is invalid.
func NewMultiSigner(options []*SignOptions) (*MultiSigner, error) {
	if len(options) == 0 {
		return nil, fmt.Errorf("dkim: no options specified")
	}

	params := make([]signParams, len(options))
	for i, opt := range options {
		p, err := checkSignOptions(opt)
		if err != nil {
			return nil, fmt.Errorf("dkim: invalid options at index %v: %v", i, err)
		}
		params[i] = p
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})

	s := &MultiSigner{
		pw:      pw,
		done:    done,
		results: make([]SignResult, len(options)),
	}

	go func() {
		defer close(done)

		fail := func(err error) {
			pr.CloseWithError(err)
			s.err = err
		}

		// Read header
		br := bufio.NewReader(pr)
		h, err := readHeader(br)
		if err != nil {
			fail(err)
			return
		}

		// Group signatures by body canonicalization algorithm, so that
		// signatures sharing the same algorithm also share the
		// canonicalization work
		type canGroup struct {
			can     io.WriteCloser
			signers []int
		}
		groups := make(map[Canonicalization]*canGroup)
		hashers := make([]hash.Hash, len(options))
		for i := range options {
			p := params[i]
			hashers[i] = p.hash.New()
			g := groups[p.bodyCan]
			if g == nil {
				g = &canGroup{}
				groups[p.bodyCan] = g
			}
			g.signers = append(g.signers, i)
		}

		bodyWriters := make([]io.Writer, 0, len(groups))
		for _, g := range groups {
			groupHashers := make([]io.Writer, len(g.signers))
			for j, i := range g.signers {
				groupHashers[j] = hashers[i]
			}
			bodyCan := params[g.signers[0]].bodyCan
			g.can = canonicalizers[bodyCan].CanonicalizeBody(io.MultiWriter(groupHashers...))
			bodyWriters = append(bodyWriters, g.can)
		}

		// Hash the body, reading it only once
		if _, err := io.Copy(io.MultiWriter(bodyWriters...), br); err != nil {
			fail(err)
			return
		}
		for _, g := range groups {
			if err := g.can.Close(); err != nil {
				fail(err)
				return
			}
		}

		// Hash and sign headers, once per signature
		for i, opt := range options {
			sigParams, err := signHeader(h, opt, params[i], hashers[i].Sum(nil))
			if err != nil {
				// A failing signature doesn't prevent the others from
				// being generated
				s.results[i].Err = err
				continue
			}
			s.results[i].Signature = formatSignature(sigParams)
		}
	}()

	return s, nil
}

// Write implements io.WriteCloser.
func (s *MultiSigner) Write(b []byte) (n int, err error) {
	return s.pw.Write(b)
}

// Close implements io.WriteCloser. It must be called once the whole message
// has been written, or to abort the operation: in both cases it waits for the
// internal pipeline to terminate. The error returned by Close must be
// checked.
func (s *MultiSigner) Close() error {
	if err := s.pw.Close(); err != nil {
		return err
	}
	<-s.done
	return s.err
}

// Results returns one SignResult per SignOptions value passed to
// NewMultiSigner, in the same order. It can only be called after a
// MultiSigner.Close call.
func (s *MultiSigner) Results() []SignResult {
	return s.results
}

// SignMultiple signs a message with multiple DKIM signatures. It reads the
// message from r once and returns one SignResult per options value, in the
// same order.
//
// The message is streamed and is not buffered in memory. The caller is
// responsible for prepending the returned signatures to the message.
func SignMultiple(r io.Reader, options []*SignOptions) ([]SignResult, error) {
	s, err := NewMultiSigner(options)
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(s, r); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	return s.Results(), nil
}
