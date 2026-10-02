package hyperloglog

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// domainVersion is the version byte of the DomainSketch envelope. It is
// distinct from every Sketch payload version so a domain-tagged encoding is
// never mistaken for a bare Sketch encoding or vice versa.
const domainVersion = 3

// ErrorDomainMismatch is returned, wrapped, when two DomainSketches with
// different hash domain identifiers are merged, when a domain-tagged binary
// is decoded against a HashConfig that does not match the recorded
// identifier, or when a sketch with an unspecified domain is merged with one
// that names a domain.
var ErrorDomainMismatch = errors.New("hash domain mismatch")

// ErrorInvalidHashConfig is returned, wrapped, when a HashConfig cannot
// identify a hash domain: the algorithm name is empty or the hash function
// is nil.
var ErrorInvalidHashConfig = errors.New("invalid hash config")

// HashConfig identifies a hash domain and carries the hash function used to
// insert elements into it. Algorithm, Seed and Domain together form the
// domain identifier: two DomainSketches may only merge when all three agree.
// Algorithm names the hash algorithm (for example "metro64"), Seed its seed,
// and Domain is an optional caller-defined label separating domains that
// share algorithm and seed.
//
// Hash is never serialized. A DomainSketch decoded from binary takes its
// Hash from the HashConfig the caller hands to UnmarshalBinaryWithConfig,
// which is rejected unless Algorithm, Seed and Domain match the recorded
// identifier.
type HashConfig struct {
	Algorithm string
	Seed      uint64
	Domain    string
	Hash      func(e []byte) uint64
}

// hashDomainID is the comparable, serializable part of a HashConfig.
type hashDomainID struct {
	algorithm string
	seed      uint64
	domain    string
}

func (c HashConfig) id() hashDomainID {
	return hashDomainID{algorithm: c.Algorithm, seed: c.Seed, domain: c.Domain}
}

func (c HashConfig) validate() error {
	if c.Algorithm == "" {
		return fmt.Errorf("hyperloglog: hash config has no algorithm name: %w", ErrorInvalidHashConfig)
	}
	if c.Hash == nil {
		return fmt.Errorf("hyperloglog: hash config for %q has no hash function: %w", c.Algorithm, ErrorInvalidHashConfig)
	}
	if len(c.Algorithm) > 255 {
		return fmt.Errorf("hyperloglog: algorithm name of %d bytes exceeds 255: %w", len(c.Algorithm), ErrorInvalidHashConfig)
	}
	if len(c.Domain) > 0xffff {
		return fmt.Errorf("hyperloglog: domain label of %d bytes exceeds 65535: %w", len(c.Domain), ErrorInvalidHashConfig)
	}
	return nil
}

// DomainSketch wraps a Sketch with the identifier of the hash domain its
// values were hashed in, so that sketches holding hashes from different
// algorithms, seeds or caller-defined domains cannot be merged by mistake.
// Construct one with NewDomainSketch; decode legacy Sketch binaries, which
// carry no domain identifier, with UnmarshalLegacyBinary.
type DomainSketch struct {
	sk  *Sketch
	cfg HashConfig
	// legacy is true when the sketch came from a bare Sketch binary and its
	// hash domain is unspecified. A legacy sketch merges only with other
	// legacy sketches, never with a named domain.
	legacy bool
}

// NewDomainSketch returns a DomainSketch with the same configuration as New
// (precision 14, sparse) whose Insert hashes with config.Hash. The config
// must name an algorithm and provide a hash function, otherwise an error
// wrapping ErrorInvalidHashConfig is returned.
func NewDomainSketch(config HashConfig) (*DomainSketch, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &DomainSketch{sk: New(), cfg: config}, nil
}

// Config returns the hash configuration of d. The returned HashConfig of a
// legacy DomainSketch is the zero value.
func (d *DomainSketch) Config() HashConfig { return d.cfg }

// Insert hashes e with the domain's hash function and adds it to d. It
// panics when d has no hash configuration, which is the case for a zero
// DomainSketch and for one decoded with UnmarshalLegacyBinary: a legacy
// sketch's domain is unspecified, so only InsertHash and Merge are
// meaningful on it.
func (d *DomainSketch) Insert(e []byte) {
	if d.legacy || d.cfg.Hash == nil {
		panic("hyperloglog: Insert on a DomainSketch without a hash configuration")
	}
	d.sk.InsertHash(d.cfg.Hash(e))
}

// InsertHash adds a uniformly distributed 64-bit hash to d. The caller
// declares by calling it on d that x belongs to d's hash domain; hashes
// produced under any other algorithm, seed or domain must not be added.
func (d *DomainSketch) InsertHash(x uint64) {
	if d.sk == nil {
		d.sk = New()
	}
	d.sk.InsertHash(x)
}

// Merge adds other to d. The merge is rejected with an error wrapping
// ErrorDomainMismatch unless both sketches name the same hash domain or both
// have an unspecified domain: a legacy sketch never matches a named domain.
// Nil and zero-value sketches are treated as empty.
func (d *DomainSketch) Merge(other *DomainSketch) error {
	if other == nil || other.sk == nil {
		return nil
	}
	if d.sk == nil {
		*d = *other.Clone()
		return nil
	}
	if d.legacy != other.legacy || (!d.legacy && d.cfg.id() != other.cfg.id()) {
		return fmt.Errorf("hyperloglog: cannot merge hash domain %q with %q: %w",
			domainName(d), domainName(other), ErrorDomainMismatch)
	}
	return d.sk.Merge(other.sk)
}

func domainName(o *DomainSketch) string {
	if o.legacy {
		return "<unspecified>"
	}
	return fmt.Sprintf("%s(seed=%d, domain=%q)", o.cfg.Algorithm, o.cfg.Seed, o.cfg.Domain)
}

// Estimate returns the cardinality estimate of d.
func (d *DomainSketch) Estimate() uint64 {
	if d.sk == nil {
		return 0
	}
	return d.sk.Estimate()
}

// Clone returns a deep copy of d.
func (d *DomainSketch) Clone() *DomainSketch {
	clone := *d
	if d.sk != nil {
		clone.sk = d.sk.Clone()
	}
	return &clone
}

// MarshalBinary implements the encoding.BinaryMarshaler interface.
func (d *DomainSketch) MarshalBinary() (data []byte, err error) {
	return d.AppendBinary(nil)
}

// AppendBinary implements the encoding.BinaryAppender interface.
//
// A DomainSketch with a named domain is encoded as an envelope: a version
// byte of 3, the length-prefixed algorithm name, the 8 byte big endian seed,
// the length-prefixed domain label, and the embedded Sketch encoding. The
// hash function is not part of the encoding; UnmarshalBinaryWithConfig
// requires the caller to supply a HashConfig whose identifier matches the
// recorded one.
//
// A legacy DomainSketch, whose domain is unspecified, has no identifier to
// record and is encoded as the bare Sketch binary it was decoded from, so
// the round trip through UnmarshalLegacyBinary preserves the format.
func (d *DomainSketch) AppendBinary(data []byte) ([]byte, error) {
	if d.legacy {
		return d.sk.AppendBinary(data)
	}
	if err := d.cfg.validate(); err != nil {
		return data, err
	}
	data = append(data, domainVersion, byte(len(d.cfg.Algorithm)))
	data = append(data, d.cfg.Algorithm...)
	data = binary.BigEndian.AppendUint64(data, d.cfg.Seed)
	data = binary.BigEndian.AppendUint16(data, uint16(len(d.cfg.Domain)))
	data = append(data, d.cfg.Domain...)
	return d.sk.AppendBinary(data)
}

// UnmarshalBinaryWithConfig decodes a domain-tagged binary produced by
// AppendBinary and adopts config as the sketch's hash configuration,
// including its hash function, which is never serialized. It returns an
// error wrapping ErrorDomainMismatch when the config's algorithm, seed or
// domain label differ from the recorded identifier, and an error wrapping
// ErrorInvalidVersion when data is a bare Sketch binary; use
// UnmarshalLegacyBinary for those. If an error is returned d is left
// unchanged.
func (d *DomainSketch) UnmarshalBinaryWithConfig(data []byte, config HashConfig) error {
	if err := config.validate(); err != nil {
		return err
	}
	if len(data) < 2 {
		return fmt.Errorf("hyperloglog: domain header needs at least 2 bytes, have %d: %w", len(data), ErrorTooShort)
	}
	if v := data[0]; v != domainVersion {
		return fmt.Errorf("hyperloglog: domain envelope version %d: %w", v, ErrorInvalidVersion)
	}
	algoLen := int(data[1])
	if algoLen == 0 {
		return fmt.Errorf("hyperloglog: domain envelope has an empty algorithm name: %w", ErrorInvalidData)
	}
	need := 2 + algoLen + 8 + 2
	if len(data) < need {
		return fmt.Errorf("hyperloglog: domain header needs %d bytes, have %d: %w", need, len(data), ErrorTooShort)
	}
	stored := hashDomainID{
		algorithm: string(data[2 : 2+algoLen]),
		seed:      binary.BigEndian.Uint64(data[2+algoLen : 2+algoLen+8]),
	}
	domainOff := 2 + algoLen + 8
	domainLen := int(binary.BigEndian.Uint16(data[domainOff : domainOff+2]))
	need += domainLen
	if len(data) < need {
		return fmt.Errorf("hyperloglog: domain header needs %d bytes, have %d: %w", need, len(data), ErrorTooShort)
	}
	stored.domain = string(data[domainOff+2 : need])

	if stored != config.id() {
		return fmt.Errorf("hyperloglog: binary records hash domain %q, config provides %q: %w",
			fmt.Sprintf("%s(seed=%d, domain=%q)", stored.algorithm, stored.seed, stored.domain),
			fmt.Sprintf("%s(seed=%d, domain=%q)", config.Algorithm, config.Seed, config.Domain),
			ErrorDomainMismatch)
	}

	sk := &Sketch{}
	if err := sk.UnmarshalBinary(data[need:]); err != nil {
		return err
	}
	d.sk = sk
	d.cfg = config
	d.legacy = false
	return nil
}

// UnmarshalLegacyBinary decodes a bare Sketch binary, as written by
// Sketch.MarshalBinary, and marks the sketch's hash domain as unspecified.
// An unspecified domain is not equal to any named domain: Merge rejects
// combining the result with any DomainSketch that has a HashConfig, and
// Insert panics because no hash function is known. Two legacy sketches merge
// with each other, as both descend from the package's historical hash.
func (d *DomainSketch) UnmarshalLegacyBinary(data []byte) error {
	sk := &Sketch{}
	if err := sk.UnmarshalBinary(data); err != nil {
		return err
	}
	d.sk = sk
	d.cfg = HashConfig{}
	d.legacy = true
	return nil
}
