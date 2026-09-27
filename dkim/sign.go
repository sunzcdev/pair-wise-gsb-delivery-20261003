package dkim

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ed25519"
)

var randReader io.Reader = rand.Reader

// SignOptions is used to configure Sign. Domain, Selector and Signer are
// mandatory.
type SignOptions struct {
	// The SDID claiming responsibility for an introduction of a message into the
	// mail stream. Hence, the SDID value is used to form the query for the public
	// key. The SDID MUST correspond to a valid DNS name under which the DKIM key
	// record is published.
	//
	// This can't be empty.
	Domain string
	// The selector subdividing the namespace for the domain.
	//
	// This can't be empty.
	Selector string
	// The Agent or User Identifier (AUID) on behalf of which the SDID is taking
	// responsibility.
	//
	// This is optional.
	Identifier string

	// The key used to sign the message.
	//
	// Supported Signer.Public() values are *rsa.PublicKey and
	// ed25519.PublicKey.
	Signer crypto.Signer
	// The hash algorithm used to sign the message. If zero, a default hash will
	// be chosen.
	//
	// The only supported hash algorithm is crypto.SHA256.
	Hash crypto.Hash

	// Header and body canonicalization algorithms.
	//
	// If empty, CanonicalizationSimple is used.
	HeaderCanonicalization Canonicalization
	BodyCanonicalization   Canonicalization

	// A list of header fields to include in the signature. If nil, all headers
	// will be included. If not nil, "From" MUST be in the list.
	//
	// See RFC 6376 section 5.4.1 for recommended header fields.
	HeaderKeys []string

	// The expiration time. A zero value means no expiration.
	Expiration time.Time

	// A list of query methods used to retrieve the public key.
	//
	// If nil, it is implicitly defined as QueryMethodDNSTXT.
	QueryMethods []QueryMethod
}

// Signer generates a DKIM signature.
//
// The whole message header and body must be written to the Signer. Close should
// always be called (either after the whole message has been written, or after
// an error occurred and the signer won't be used anymore). Close may return an
// error in case signing fails.
//
// After a successful Close, Signature can be called to retrieve the
// DKIM-Signature header field that the caller should prepend to the message.
type Signer struct {
	pw        *io.PipeWriter
	done      <-chan error
	sigParams map[string]string // only valid after done received nil
}

// signConfig holds the normalized signing parameters derived from
// SignOptions.
type signConfig struct {
	headerCan Canonicalization
	bodyCan   Canonicalization
	keyAlgo   string
	hash      crypto.Hash
	hashAlgo  string
}

func checkSignOptions(options *SignOptions) (*signConfig, error) {
	if options == nil {
		return nil, fmt.Errorf("dkim: no options specified")
	}
	if options.Domain == "" {
		return nil, fmt.Errorf("dkim: no domain specified")
	}
	if options.Selector == "" {
		return nil, fmt.Errorf("dkim: no selector specified")
	}
	if options.Signer == nil {
		return nil, fmt.Errorf("dkim: no signer specified")
	}

	headerCan := options.HeaderCanonicalization
	if headerCan == "" {
		headerCan = CanonicalizationSimple
	}
	if _, ok := canonicalizers[headerCan]; !ok {
		return nil, fmt.Errorf("dkim: unknown header canonicalization %q", headerCan)
	}

	bodyCan := options.BodyCanonicalization
	if bodyCan == "" {
		bodyCan = CanonicalizationSimple
	}
	if _, ok := canonicalizers[bodyCan]; !ok {
		return nil, fmt.Errorf("dkim: unknown body canonicalization %q", bodyCan)
	}

	var keyAlgo string
	switch options.Signer.Public().(type) {
	case *rsa.PublicKey:
		keyAlgo = "rsa"
	case ed25519.PublicKey:
		keyAlgo = "ed25519"
	default:
		return nil, fmt.Errorf("dkim: unsupported key algorithm %T", options.Signer.Public())
	}

	hash := options.Hash
	var hashAlgo string
	switch options.Hash {
	case 0: // sha256 is the default
		hash = crypto.SHA256
		fallthrough
	case crypto.SHA256:
		hashAlgo = "sha256"
	case crypto.SHA1:
		return nil, fmt.Errorf("dkim: hash algorithm too weak: sha1")
	default:
		return nil, fmt.Errorf("dkim: unsupported hash algorithm")
	}

	if options.HeaderKeys != nil {
		ok := false
		for _, k := range options.HeaderKeys {
			if strings.EqualFold(k, "From") {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("dkim: the From header field must be signed")
		}
	}

	return &signConfig{
		headerCan: headerCan,
		bodyCan:   bodyCan,
		keyAlgo:   keyAlgo,
		hash:      hash,
		hashAlgo:  hashAlgo,
	}, nil
}

// computeSignature signs the already-parsed header and the already-computed
// body hash with the provided options.
func computeSignature(h header, options *SignOptions, cfg *signConfig, bodyHashed []byte) (map[string]string, error) {
	params := map[string]string{
		"v":  "1",
		"a":  cfg.keyAlgo + "-" + cfg.hashAlgo,
		"bh": base64.StdEncoding.EncodeToString(bodyHashed),
		"c":  string(cfg.headerCan) + "/" + string(cfg.bodyCan),
		"d":  options.Domain,
		//"l": "", // TODO
		"s": options.Selector,
		"t": formatTime(now()),
		//"z": "", // TODO
	}

	var headerKeys []string
	if options.HeaderKeys != nil {
		headerKeys = options.HeaderKeys
	} else {
		for _, kv := range h {
			k, _ := parseHeaderField(kv)
			headerKeys = append(headerKeys, k)
		}
	}
	params["h"] = formatTagList(headerKeys)

	if options.Identifier != "" {
		params["i"] = options.Identifier
	}

	if options.QueryMethods != nil {
		methods := make([]string, len(options.QueryMethods))
		for i, method := range options.QueryMethods {
			methods[i] = string(method)
		}
		params["q"] = formatTagList(methods)
	}

	if !options.Expiration.IsZero() {
		params["x"] = formatTime(options.Expiration)
	}

	// Hash and sign headers
	hasher := cfg.hash.New()
	picker := newHeaderPicker(h)
	for _, k := range headerKeys {
		kv := picker.Pick(k)
		if kv == "" {
			// The Signer MAY include more instances of a header field name
			// in "h=" than there are actual corresponding header fields so
			// that the signature will not verify if additional header
			// fields of that name are added.
			continue
		}

		kv = canonicalizers[cfg.headerCan].CanonicalizeHeader(kv)
		if _, err := io.WriteString(hasher, kv); err != nil {
			return nil, err
		}
	}

	params["b"] = ""
	sigField := formatSignature(params)
	sigField = canonicalizers[cfg.headerCan].CanonicalizeHeader(sigField)
	sigField = strings.TrimRight(sigField, crlf)
	if _, err := io.WriteString(hasher, sigField); err != nil {
		return nil, err
	}
	hashed := hasher.Sum(nil)

	// Don't pass Hash to Sign for ed25519 as it doesn't support it
	// and will return an error ("ed25519: cannot sign hashed message").
	hash := cfg.hash
	if cfg.keyAlgo == "ed25519" {
		hash = crypto.Hash(0)
	}

	sig, err := options.Signer.Sign(randReader, hashed, hash)
	if err != nil {
		return nil, err
	}
	params["b"] = base64.StdEncoding.EncodeToString(sig)

	return params, nil
}

// NewSigner creates a new signer. It returns an error if SignOptions is
// invalid.
func NewSigner(options *SignOptions) (*Signer, error) {
	cfg, err := checkSignOptions(options)
	if err != nil {
		return nil, err
	}

	done := make(chan error, 1)
	pr, pw := io.Pipe()

	s := &Signer{
		pw:   pw,
		done: done,
	}

	closeReadWithError := func(err error) {
		pr.CloseWithError(err)
		done <- err
	}

	go func() {
		defer close(done)

		// Read header
		br := bufio.NewReader(pr)
		h, err := readHeader(br)
		if err != nil {
			closeReadWithError(err)
			return
		}

		// Hash body
		hasher := cfg.hash.New()
		can := canonicalizers[cfg.bodyCan].CanonicalizeBody(hasher)
		if _, err := io.Copy(can, br); err != nil {
			closeReadWithError(err)
			return
		}
		if err := can.Close(); err != nil {
			closeReadWithError(err)
			return
		}

		params, err := computeSignature(h, options, cfg, hasher.Sum(nil))
		if err != nil {
			closeReadWithError(err)
			return
		}

		s.sigParams = params
		closeReadWithError(nil)
	}()

	return s, nil
}

// Write implements io.WriteCloser.
func (s *Signer) Write(b []byte) (n int, err error) {
	return s.pw.Write(b)
}

// Close implements io.WriteCloser. The error return by Close must be checked.
func (s *Signer) Close() error {
	if err := s.pw.Close(); err != nil {
		return err
	}
	return <-s.done
}

// Signature returns the whole DKIM-Signature header field. It can only be
// called after a successful Signer.Close call.
//
// The returned value contains both the header field name, its value and the
// final CRLF.
func (s *Signer) Signature() string {
	if s.sigParams == nil {
		panic("dkim: Signer.Signature must only be called after a succesful Signer.Close")
	}
	return formatSignature(s.sigParams)
}

// Sign signs a message. It reads it from r and writes the signed version to w.
func Sign(w io.Writer, r io.Reader, options *SignOptions) error {
	s, err := NewSigner(options)
	if err != nil {
		return err
	}
	defer s.Close()

	// We need to keep the message in a buffer so we can write the new DKIM
	// header field before the rest of the message
	var b bytes.Buffer
	mw := io.MultiWriter(&b, s)

	if _, err := io.Copy(mw, r); err != nil {
		return err
	}
	if err := s.Close(); err != nil {
		return err
	}

	if _, err := io.WriteString(w, s.Signature()); err != nil {
		return err
	}
	_, err = io.Copy(w, &b)
	return err
}

// A SignResult holds the outcome of signing a message with one of the
// SignOptions passed to SignMultiple.
type SignResult struct {
	// Signature is the whole DKIM-Signature header field that the caller
	// should prepend to the message. It contains both the header field
	// name, its value and the final CRLF. It is empty if Err is non-nil.
	Signature string
	// Err is set when signing with this entry's SignOptions failed (e.g.
	// its crypto.Signer returned an error). It doesn't affect the other
	// entries.
	Err error
}

// SignMultiple signs a message with multiple sets of options in a single
// pass over the message. It is typically used during key rotation, when a
// message must carry signatures for both the old and the new selector.
//
// The message body is read from r exactly once. Options entries which use
// the same hash algorithm and body canonicalization share the same body
// canonicalization and hashing work. Memory usage doesn't depend on the
// message size, so SignMultiple is suitable for messages with large
// attachments.
//
// The returned slice contains one SignResult per options entry, in the same
// order. If signing fails for a single entry, the other entries are still
// processed and the failure is reported in that entry's SignResult.Err. An
// error is only returned directly for failures which prevent signing as a
// whole: invalid options, or a failure to read the message from r.
//
// SignMultiple doesn't spawn any background goroutine: if the caller stops
// reading from its source or r returns an error, SignMultiple returns
// immediately without leaving any internal pipeline behind.
//
// The returned signatures should be prepended to the message header, in
// order.
func SignMultiple(r io.Reader, optionsList []*SignOptions) ([]*SignResult, error) {
	if len(optionsList) == 0 {
		return nil, fmt.Errorf("dkim: no options specified")
	}

	cfgs := make([]*signConfig, len(optionsList))
	for i, options := range optionsList {
		cfg, err := checkSignOptions(options)
		if err != nil {
			return nil, err
		}
		cfgs[i] = cfg
	}

	// Read header
	br := bufio.NewReader(r)
	h, err := readHeader(br)
	if err != nil {
		return nil, err
	}

	// Group options by hash algorithm and body canonicalization, so that
	// entries sharing them also share the canonicalization and hashing work.
	type bodyKey struct {
		hash crypto.Hash
		can  Canonicalization
	}
	type bodyGroup struct {
		hasher hash.Hash
		can    io.WriteCloser
		sum    []byte
	}
	groups := make(map[bodyKey]*bodyGroup)
	for _, cfg := range cfgs {
		key := bodyKey{cfg.hash, cfg.bodyCan}
		if _, ok := groups[key]; !ok {
			hasher := cfg.hash.New()
			groups[key] = &bodyGroup{
				hasher: hasher,
				can:    canonicalizers[cfg.bodyCan].CanonicalizeBody(hasher),
			}
		}
	}

	// Hash the body, streaming it through each group's canonicalizer
	writers := make([]io.Writer, 0, len(groups))
	for _, g := range groups {
		writers = append(writers, g.can)
	}
	if _, err := io.Copy(io.MultiWriter(writers...), br); err != nil {
		return nil, err
	}
	for _, g := range groups {
		if err := g.can.Close(); err != nil {
			return nil, err
		}
		g.sum = g.hasher.Sum(nil)
	}

	// Sign the header for each options entry independently, so that a
	// failing crypto.Signer doesn't prevent the other signatures from being
	// generated
	results := make([]*SignResult, len(optionsList))
	for i, options := range optionsList {
		res := new(SignResult)
		results[i] = res

		key := bodyKey{cfgs[i].hash, cfgs[i].bodyCan}
		params, err := computeSignature(h, options, cfgs[i], groups[key].sum)
		if err != nil {
			res.Err = err
			continue
		}
		res.Signature = formatSignature(params)
	}
	return results, nil
}

func formatSignature(params map[string]string) string {
	sig := formatHeaderParams(headerFieldName, params)
	return sig
}

func formatTagList(l []string) string {
	return strings.Join(l, ":")
}

func formatTime(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
