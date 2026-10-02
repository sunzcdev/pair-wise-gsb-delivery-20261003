package vm

// mask is a bitset marking rows that are currently "active" (not masked out).
// An inactive row skips every instruction until a control-flow opcode merges it
// back in, which mirrors per-row short-circuit behaviour.
type mask struct {
	bits []uint64
}

func newMask(n int, active bool) mask {
	m := mask{bits: make([]uint64, (n+63)/64)}
	if active {
		for i := range m.bits {
			m.bits[i] = ^uint64(0)
		}
		if rem := n % 64; rem != 0 {
			m.bits[len(m.bits)-1] &= 1<<uint(rem) - 1
		}
	}
	return m
}

func (m mask) get(i int) bool {
	return m.bits[i>>6]&(1<<uint(i&63)) != 0
}

func (m mask) set(i int) {
	m.bits[i>>6] |= 1 << uint(i&63)
}

func (m mask) clear(i int) {
	m.bits[i>>6] &^= 1 << uint(i&63)
}

// or enables every row that is active in other.
func (m mask) or(other mask) {
	for i := range m.bits {
		m.bits[i] |= other.bits[i]
	}
}

// andNot disables every row active in other.
func (m mask) andNot(other mask) {
	for i := range m.bits {
		m.bits[i] &^= other.bits[i]
	}
}

func (m mask) any() bool {
	for _, w := range m.bits {
		if w != 0 {
			return true
		}
	}
	return false
}

func (m mask) clone() mask {
	bits := make([]uint64, len(m.bits))
	copy(bits, m.bits)
	return mask{bits}
}
