package internalapi

type WazeroOnly interface {
	wazeroOnly()
}

type WazeroOnlyType struct{}

func (WazeroOnlyType) wazeroOnly() {}

// MemoryGeneration is the internal backing type for api.MemoryGeneration. It
// is aliased into the api package so that the value carried by the publicly
// visible token stays opaque to callers, while wazero's own api.Memory
// implementations can construct and inspect it.
type MemoryGeneration struct {
	// grown is the number of successful, non-zero grows observed at the time
	// the token was produced. Zero means "never grown"; NewMemoryGeneration
	// shifts the externally visible value by one so the zero value is
	// reserved.
	grown uint64
}

// NewMemoryGeneration builds a MemoryGeneration from the number of
// successful, non-zero memory grows observed so far.
func NewMemoryGeneration(grown uint64) MemoryGeneration {
	return MemoryGeneration{grown: grown + 1}
}

// GrownMemoryGeneration returns the grow counter embedded in a token produced
// by NewMemoryGeneration.
func GrownMemoryGeneration(gen MemoryGeneration) uint64 {
	return gen.grown - 1
}
