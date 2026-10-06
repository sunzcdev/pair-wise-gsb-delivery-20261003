package main

import (
	"crypto/tls"
	"sync"
)

// CertStorage is a simple certificate cache that keeps
// everything in memory.
type CertStorage struct {
	certs map[uint64]map[string]*tls.Certificate
	mtx   sync.RWMutex
}

// Fetch keeps CertStorage compatible with the original goproxy.CertStorage
// interface. It is only reached when the proxy is not generation aware; those
// entries are stored under generation 0.
func (cs *CertStorage) Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	return cs.FetchGeneration(hostname, 0, gen)
}

// FetchGeneration satisfies goproxy.GenerationCertStorage. Certificates are
// cached per CA generation, so rotating the signing CA never serves a leaf
// certificate signed by the old CA after the switch.
func (cs *CertStorage) FetchGeneration(hostname string, generation uint64, gen func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	cs.mtx.RLock()
	cert, ok := cs.certs[generation][hostname]
	cs.mtx.RUnlock()
	if ok {
		return cert, nil
	}

	cert, err := gen()
	if err != nil {
		return nil, err
	}

	cs.mtx.Lock()
	if cs.certs[generation] == nil {
		cs.certs[generation] = make(map[string]*tls.Certificate)
	}
	cs.certs[generation][hostname] = cert
	cs.mtx.Unlock()

	return cert, nil
}

// DropGeneration satisfies goproxy.GenerationCertRecycler. It releases every
// certificate cached for a CA generation that has been rotated away, keeping
// memory usage flat across repeated rotations.
func (cs *CertStorage) DropGeneration(generation uint64) {
	cs.mtx.Lock()
	delete(cs.certs, generation)
	cs.mtx.Unlock()
}

func NewCertStorage() *CertStorage {
	return &CertStorage{
		certs: make(map[uint64]map[string]*tls.Certificate),
	}
}
