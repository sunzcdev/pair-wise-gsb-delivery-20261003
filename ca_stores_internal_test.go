package goproxy

import (
	"crypto/tls"
	"sync"
	"sync/atomic"
)

// blockingGenerationStore blocks the generation selected by blockGen inside
// FetchGeneration (after the cache miss, before signing) to emulate a signing
// task that is still in flight when the CA rotates. It deliberately does not
// implement GenerationCertRecycler so the test can inspect every generation's
// entries after the fact.
type blockingGenerationStore struct {
	mu          sync.Mutex
	certs       map[uint64]map[string]*tls.Certificate
	enteredGen1 chan struct{}
	releaseGen1 chan struct{}
	enteredOnce sync.Once
	blockGen    atomic.Uint64
}

func (s *blockingGenerationStore) Fetch(
	hostname string,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	return s.FetchGeneration(hostname, 0, gen)
}

func (s *blockingGenerationStore) FetchGeneration(
	hostname string,
	generation uint64,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	s.mu.Lock()
	if byGen, ok := s.certs[generation]; ok {
		if cert, found := byGen[hostname]; found {
			s.mu.Unlock()
			return cert, nil
		}
	}
	s.mu.Unlock()

	if s.blockGen.Load() == generation {
		s.enteredOnce.Do(func() { close(s.enteredGen1) })
		<-s.releaseGen1
	}

	cert, err := gen()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.certs[generation] == nil {
		s.certs[generation] = make(map[string]*tls.Certificate)
	}
	s.certs[generation][hostname] = cert
	s.mu.Unlock()
	return cert, nil
}

// legacyStore implements only the original CertStorage interface. The proxy
// must keep it correct across rotations by namespacing keys itself.
type legacyStore struct {
	mu    sync.Mutex
	certs map[string]*tls.Certificate
}

func newLegacyStore() *legacyStore {
	return &legacyStore{certs: make(map[string]*tls.Certificate)}
}

func (s *legacyStore) Fetch(
	hostname string,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	s.mu.Lock()
	if cert, ok := s.certs[hostname]; ok {
		s.mu.Unlock()
		return cert, nil
	}
	s.mu.Unlock()

	cert, err := gen()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.certs[hostname] = cert
	s.mu.Unlock()
	return cert, nil
}

// trackingGenerationStore records which generations were reclaimed via
// DropGeneration.
type trackingGenerationStore struct {
	mu          sync.Mutex
	certs       map[uint64]map[string]*tls.Certificate
	droppedGens map[uint64]bool
}

func newTrackingGenerationStore() *trackingGenerationStore {
	return &trackingGenerationStore{
		certs:       make(map[uint64]map[string]*tls.Certificate),
		droppedGens: make(map[uint64]bool),
	}
}

func (s *trackingGenerationStore) Fetch(
	hostname string,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	return s.FetchGeneration(hostname, 0, gen)
}

func (s *trackingGenerationStore) FetchGeneration(
	hostname string,
	generation uint64,
	gen func() (*tls.Certificate, error),
) (*tls.Certificate, error) {
	s.mu.Lock()
	if byGen, ok := s.certs[generation]; ok {
		if cert, found := byGen[hostname]; found {
			s.mu.Unlock()
			return cert, nil
		}
	}
	s.mu.Unlock()

	cert, err := gen()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.certs[generation] == nil {
		s.certs[generation] = make(map[string]*tls.Certificate)
	}
	s.certs[generation][hostname] = cert
	s.mu.Unlock()
	return cert, nil
}

func (s *trackingGenerationStore) DropGeneration(generation uint64) {
	s.mu.Lock()
	s.droppedGens[generation] = true
	delete(s.certs, generation)
	s.mu.Unlock()
}

func (s *trackingGenerationStore) dropped(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.droppedGens[generation]
}
