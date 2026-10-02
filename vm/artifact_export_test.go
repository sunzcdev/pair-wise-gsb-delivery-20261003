package vm

// Test-only accessors for constructing malformed artifacts in package
// vm_test tests.

// ArtifactHeaderLen is the fixed container header length.
const ArtifactHeaderLen = len(artifactMagic) + 4 + 4 + 4

// ArtifactPayloadRange returns [start, end) byte offsets of the payload
// section inside a serialized artifact.
func ArtifactPayloadRange(data []byte) (int, int) {
	n := int(uint32(data[16])<<24 | uint32(data[17])<<16 | uint32(data[18])<<8 | uint32(data[19]))
	return ArtifactHeaderLen, ArtifactHeaderLen + n
}

// ArtifactChecksumAlgo marks that tests should recompute SHA-256 over
// [start,end) and copy the 32-byte digest at end.

// ArtifactBytecodeOffset returns the payload offset of the first bytecode
// byte and the number of bytecode instructions recorded in the header.
func ArtifactBytecodeOffset(payload []byte) (offset, count int) {
	// payload = source(uvarint-length-prefixed) | count:u32 | code...
	i := 0
	srcLen, n := readTestUvarint(payload, i)
	i += n + int(srcLen) // length prefix plus source contents
	count = int(uint32(payload[i])<<24 | uint32(payload[i+1])<<16 | uint32(payload[i+2])<<8 | uint32(payload[i+3]))
	return i + 4, count
}

func readTestUvarint(b []byte, at int) (value uint64, n int) {
	for shift := uint(0); ; shift += 7 {
		c := b[at+n]
		n++
		value |= uint64(c&0x7F) << shift
		if c&0x80 == 0 {
			return value, n
		}
	}
}

// ArtifactInstructionOffset returns the payload offset of instruction index
// within a bytecode section starting at codeOffset.
func ArtifactInstructionOffset(payload []byte, codeOffset, index int) int {
	off := codeOffset
	for i := 0; i < index; i++ {
		off++ // opcode byte
		for payload[off]&0x80 != 0 {
			off++
		}
		off++ // last argument byte
	}
	return off
}
