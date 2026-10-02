package hyperloglog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"

	metro "github.com/dgryski/go-metro"
)

const (
	defaultHashAlgorithm = "metro64"
	defaultHashSeed      = uint64(1337)

	domainBinaryMagic = 0xd0
	domainBinaryVer   = byte(1)
	// 2 magic/version bytes, 8 byte domain ID, 8 byte seed, 1 byte algorithm
	// length and 1 byte domain length.
	domainHeaderSize = 2 + 8 + 8 + 1 + 1
)

// HashFunc maps an element to a uniformly distributed 64 bit hash. It must be
// deterministic and depend only on e.
type HashFunc func(e []byte) uint64

// HashConfig identifies a hash domain: which hash function produced the values
// stored in a sketch. Algorithm is a non empty name such as "metro64"; Seed is
// the hash seed (0 is a valid seed); Domain is an additional caller chosen
// identifier used to separate namespaces that share Algorithm and Seed, for
// example the name of the metric being counted. Hash holds the function used
// to hash Insert inputs and is never serialized: the deserializing side
// supplies a config carrying the matching function.
//
// Two sketches are in the same hash domain only if Algorithm, Seed and Domain
// all agree. The zero HashConfig refers to the package default domain,
// MetroHash64 with seed 1337 and an empty domain tag.
type HashConfig struct {
	Algorithm string
	Seed      uint64
	Domain    string
	Hash      HashFunc
}

// MetroHashConfig returns a HashConfig for MetroHash64 with the given seed and
// caller chosen domain tag. domain must be non empty unless the result is the
// package default (seed 1337 and an empty tag): unnamed non default sketches
// cannot be told apart from the package default domain.
func MetroHashConfig(seed uint64, domain string) HashConfig {
	return HashConfig{Algorithm: defaultHashAlgorithm, Seed: seed, Domain: domain}
}

// CustomHashConfig returns a HashConfig for a caller supplied hash. algorithm
// names the algorithm, domain is a non empty caller chosen tag, and hash is
// the hash function itself, which is not serialized.
func CustomHashConfig(algorithm, domain string, hash HashFunc) HashConfig {
	return HashConfig{Algorithm: algorithm, Domain: domain, Hash: hash}
}

func metroHash(seed uint64) HashFunc {
	return func(e []byte) uint64 { return metro.Hash64(e, seed) }
}

// normalized fills defaults, installs the built in hash implementation, and
// validates the config.
func (c HashConfig) normalized() (HashConfig, error) {
	if c.Algorithm == "" && c.Hash == nil && c.Seed == 0 && c.Domain == "" {
		c.Algorithm = defaultHashAlgorithm
		c.Seed = defaultHashSeed
	}
	if c.Algorithm == "" {
		return HashConfig{}, fmt.Errorf("hyperloglog: hash config needs an algorithm name: %w", ErrorInvalidHashConfig)
	}
	if c.Hash == nil {
		if c.Algorithm != defaultHashAlgorithm {
			return HashConfig{}, fmt.Errorf("hyperloglog: hash function for algorithm %q must be supplied: %w", c.Algorithm, ErrorInvalidHashConfig)
		}
		c.Hash = metroHash(c.Seed)
	}
	if len(c.Algorithm) > 0xff || len(c.Domain) > 0xff {
		return HashConfig{}, fmt.Errorf("hyperloglog: algorithm or domain tag longer than %d bytes: %w", 0xff, ErrorInvalidHashConfig)
	}
	return c, nil
}

// DomainID identifies a hash domain. It is derived from Algorithm, Seed and
// Domain, never from the Hash function pointer.
type DomainID uint64

// UnspecifiedDomain marks data whose hash domain is not recorded, namely
// plain Sketch binary produced before domains existed. It never compares
// equal to a config based ID: HashConfig.id never emits zero.
const UnspecifiedDomain DomainID = 0

// id derives the domain identifier with 64 bit FNV-1a over the normalized
// config, making accidental collisions between distinct configs negligible.
func (c HashConfig) id() DomainID {
	h := fnv.New64a()
	var buf [8]byte
	write := func(b []byte) {
		binary.LittleEndian.PutUint64(buf[:], uint64(len(b)))
		h.Write(buf[:])
		h.Write(b)
	}
	write([]byte(c.Algorithm))
	binary.LittleEndian.PutUint64(buf[:], c.Seed)
	h.Write(buf[:])
	write([]byte(c.Domain))
	return DomainID(h.Sum64())
}

// DomainSketch is a Sketch bound to an explicit hash domain. Construct one
// with NewDomainSketch or UnmarshalDomainBinary; its zero value is not usable.
// InsertHash values have to come from the domain's hash, and Merge rejects
// sketches from another domain. Legacy sketches loaded with
// LegacyDomainSketch are marked as carrying an unspecified domain and only
// merge with each other.
type DomainSketch struct {
	sk  *Sketch
	cfg HashConfig
	id  DomainID
}

// NewDomainSketch returns a domain aware sketch. config.Hash may be nil for
// the built in "metro64" algorithm; for every other algorithm the caller has
// to supply the hash. A non default config must carry a non empty Domain so
// it cannot be confused with the package default domain.
func NewDomainSketch(precision uint8, sparse bool, config HashConfig) (*DomainSketch, error) {
	cfg, err := config.normalized()
	if err != nil {
		return nil, err
	}
	if cfg.Domain == "" && (cfg.Algorithm != defaultHashAlgorithm || cfg.Seed != defaultHashSeed) {
		return nil, fmt.Errorf("hyperloglog: non default hash config needs a non empty domain tag: %w", ErrorInvalidHashConfig)
	}
	sk, err := NewSketch(precision, sparse)
	if err != nil {
		return nil, err
	}
	return &DomainSketch{sk: sk, cfg: cfg, id: cfg.id()}, nil
}

// Precision returns the sketch's precision.
func (d *DomainSketch) Precision() uint8 { return d.sk.p }

// Estimate returns the cardinality estimate.
func (d *DomainSketch) Estimate() uint64 { return d.sk.Estimate() }

// Domain returns the domain identifier Merge compares.
func (d *DomainSketch) Domain() DomainID { return d.id }

// HashConfig returns the normalized config the sketch was created or
// deserialized with.
func (d *DomainSketch) HashConfig() HashConfig { return d.cfg }

// IsLegacy reports whether d holds data loaded through LegacyDomainSketch
// whose hash domain was never recorded.
func (d *DomainSketch) IsLegacy() bool { return d.id == UnspecifiedDomain }

// Insert hashes e with the domain's hash function and adds it.
func (d *DomainSketch) Insert(e []byte) { d.sk.InsertHash(d.cfg.Hash(e)) }

// InsertHash adds a hash the caller declares to belong to d's domain. Values
// from another domain corrupt the estimate; the caller is responsible for
// computing x with the HashConfig d was configured with.
func (d *DomainSketch) InsertHash(x uint64) { d.sk.InsertHash(x) }

// Merge adds other to d. Sketches with different domains are rejected, and
// legacy sketches without a recorded domain only merge with other legacy
// sketches: unmarked data is never assumed to match a declared domain.
func (d *DomainSketch) Merge(other *DomainSketch) error {
	if d.id != other.id {
		return fmt.Errorf("hyperloglog: cannot merge hash domain %s with %s: %w",
			domainLabel(d.id, d.cfg), domainLabel(other.id, other.cfg), ErrorDomainMismatch)
	}
	return d.sk.Merge(other.sk)
}

func domainLabel(id DomainID, cfg HashConfig) string {
	if id == UnspecifiedDomain {
		return "unspecified (legacy encoding)"
	}
	if cfg.Domain != "" {
		return fmt.Sprintf("%q/%s/seed=%d", cfg.Domain, cfg.Algorithm, cfg.Seed)
	}
	return fmt.Sprintf("%s/seed=%d", cfg.Algorithm, cfg.Seed)
}

// Clone returns a deep copy of d.
func (d *DomainSketch) Clone() *DomainSketch {
	return &DomainSketch{sk: d.sk.Clone(), cfg: d.cfg, id: d.id}
}

// Reset empties the sketch without changing its domain.
func (d *DomainSketch) Reset() { d.sk.Reset() }

// LegacyDomainSketch decodes plain Sketch binary in the v1 or v2 format
// produced by Sketch.MarshalBinary. The result is explicitly marked as
// carrying an unspecified domain, never as the package default domain: it
// merges only with other legacy sketches.
func LegacyDomainSketch(data []byte) (*DomainSketch, error) {
	sk := &Sketch{}
	if err := sk.UnmarshalBinary(data); err != nil {
		return nil, err
	}
	return legacyFromSketch(sk), nil
}

func legacyFromSketch(sk *Sketch) *DomainSketch {
	return &DomainSketch{
		sk:  sk,
		cfg: HashConfig{Algorithm: defaultHashAlgorithm, Seed: defaultHashSeed, Hash: metroHash(defaultHashSeed)},
		id:  UnspecifiedDomain,
	}
}

// MarshalBinary implements encoding.BinaryMarshaler. The hash function is not
// serialized; UnmarshalBinary has to be handed a config carrying a matching
// one. Legacy sketches are written in the plain Sketch v2 format so existing
// readers keep accepting them.
func (d *DomainSketch) MarshalBinary() ([]byte, error) {
	return d.AppendBinary(nil)
}

// AppendBinary implements the encoding.BinaryAppender interface.
func (d *DomainSketch) AppendBinary(data []byte) ([]byte, error) {
	if d.id == UnspecifiedDomain {
		return d.sk.AppendBinary(data)
	}
	inner, err := d.sk.AppendBinary(nil)
	if err != nil {
		return data, err
	}
	data = slices.Grow(data, domainHeaderSize+len(inner)+4)
	data = append(data, domainBinaryMagic, domainBinaryVer)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(d.id))
	data = append(data, buf[:]...)
	binary.BigEndian.PutUint64(buf[:], d.cfg.Seed)
	data = append(data, buf[:]...)
	data = append(data, byte(len(d.cfg.Algorithm)), byte(len(d.cfg.Domain)))
	data = append(data, d.cfg.Algorithm...)
	data = append(data, d.cfg.Domain...)
	binary.BigEndian.PutUint32(buf[:4], uint32(len(inner)))
	data = append(data, buf[:4]...)
	data = append(data, inner...)
	return data, nil
}

// UnmarshalBinary implements encoding.BinaryUnmarshaler. d must have been
// constructed with NewDomainSketch from the config the data was written with;
// that config supplies the hash function and is checked against the recorded
// Algorithm, Seed and Domain. Plain legacy Sketch encoding is rejected with
// ErrorLegacyEncoding; use LegacyDomainSketch for it. Trailing bytes return
// ErrorInvalidData.
func (d *DomainSketch) UnmarshalBinary(data []byte) error {
	if d.cfg.Hash == nil {
		return fmt.Errorf("hyperloglog: unmarshal needs a sketch preconfigured with the matching HashConfig: %w", ErrorInvalidHashConfig)
	}
	out, err := unmarshalDomain(data, d.cfg)
	if err != nil {
		return err
	}
	*d = *out
	return nil
}

// UnmarshalDomainBinary decodes the domain aware format. config has to match
// the encoding's Algorithm, Seed and Domain and supplies the hash function,
// which is never serialized. Plain legacy Sketch encoding is rejected with
// ErrorLegacyEncoding; use LegacyDomainSketch for it.
func UnmarshalDomainBinary(data []byte, config HashConfig) (*DomainSketch, error) {
	cfg, err := config.normalized()
	if err != nil {
		return nil, err
	}
	return unmarshalDomain(data, cfg)
}

func unmarshalDomain(data []byte, cfg HashConfig) (*DomainSketch, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("hyperloglog: domain header needs %d bytes, have 0: %w", domainHeaderSize, ErrorTooShort)
	}
	if data[0] != domainBinaryMagic {
		if data[0] == 1 || data[0] == 2 {
			return nil, fmt.Errorf("hyperloglog: plain sketch encoding without a hash domain, use LegacyDomainSketch: %w", ErrorLegacyEncoding)
		}
		return nil, fmt.Errorf("hyperloglog: domain magic byte %#x: %w", data[0], ErrorInvalidVersion)
	}
	if len(data) < domainHeaderSize {
		return nil, fmt.Errorf("hyperloglog: domain header needs %d bytes, have %d: %w", domainHeaderSize, len(data), ErrorTooShort)
	}
	if data[1] != domainBinaryVer {
		return nil, fmt.Errorf("hyperloglog: domain encoding version %d: %w", data[1], ErrorInvalidVersion)
	}
	off := 2
	id := DomainID(binary.BigEndian.Uint64(data[off:]))
	off += 8
	seed := binary.BigEndian.Uint64(data[off:])
	off += 8
	algLen := int(data[off])
	domLen := int(data[off+1])
	off += 2
	if len(data) < off+algLen+domLen+4 {
		return nil, fmt.Errorf("hyperloglog: domain descriptor truncated: %w", ErrorTooShort)
	}
	algorithm := string(data[off : off+algLen])
	off += algLen
	domain := string(data[off : off+domLen])
	off += domLen

	if algorithm != cfg.Algorithm || seed != cfg.Seed || domain != cfg.Domain {
		return nil, fmt.Errorf("hyperloglog: encoding domain %q/%s/seed=%d does not match configured %q/%s/seed=%d: %w",
			domain, algorithm, seed, cfg.Domain, cfg.Algorithm, cfg.Seed, ErrorDomainMismatch)
	}
	if want := cfg.id(); id != want {
		return nil, fmt.Errorf("hyperloglog: domain ID %#016x does not match descriptor %#016x: %w", id, want, ErrorInvalidData)
	}

	innerLen := int(binary.BigEndian.Uint32(data[off:]))
	off += 4
	if len(data[off:]) < innerLen {
		return nil, fmt.Errorf("hyperloglog: inner sketch needs %d bytes, have %d: %w", innerLen, len(data[off:]), ErrorTooShort)
	}
	sk := &Sketch{}
	if err := sk.UnmarshalBinary(data[off : off+innerLen]); err != nil {
		return nil, err
	}
	if len(data[off:]) != innerLen {
		return nil, fmt.Errorf("hyperloglog: %d trailing bytes after inner sketch: %w", len(data[off:])-innerLen, ErrorInvalidData)
	}
	return &DomainSketch{sk: sk, cfg: cfg, id: id}, nil
}

// ErrorInvalidHashConfig is returned, wrapped, when a HashConfig is missing an
// algorithm name or hash function, or when a non default config omits its
// domain tag.
var ErrorInvalidHashConfig = errors.New("invalid hash config")

// ErrorDomainMismatch is wrapped by Merge and the domain unmarshalers when two
// hash domains do not agree, or when an encoding does not match the config the
// caller supplied.
var ErrorDomainMismatch = errors.New("hash domains must be equal")

// ErrorLegacyEncoding is wrapped by the domain unmarshalers when the data is
// plain Sketch binary without a recorded hash domain. Such data has to be
// loaded with LegacyDomainSketch and stays marked as an unspecified domain.
var ErrorLegacyEncoding = errors.New("legacy sketch encoding without hash domain")
