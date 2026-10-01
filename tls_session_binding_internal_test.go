package ftpserver

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/stretchr/testify/require"
)

func testAEAD(t *testing.T) cipher.AEAD {
	t.Helper()

	block, err := aes.NewCipher(make([]byte, 32))
	require.NoError(t, err)

	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)

	return aead
}

func TestSessionBindingTicketRoundTrip(t *testing.T) {
	binding, err := newTLSSessionBinding(testAEAD(t))
	require.NoError(t, err)
	require.Len(t, binding.tag, sessionBindingTagLen)

	_, _, err = binding.openTicket([]byte("not a ticket"))
	require.Error(t, err)

	// Round-trip a tag through Extra without depending on a real TLS session:
	// Extra replacement must be idempotent across ticket rotations.
	extras := replaceBindingTagExtra(nil, binding.tag)
	require.Equal(t, binding.tag, findBindingTagExtra(extras))

	// Re-appending must keep a single entry and preserve the newest tag.
	other := make([]byte, sessionBindingTagLen)
	other[0] = 1
	extras = replaceBindingTagExtra(extras, other)
	require.Len(t, extras, 1)
	require.Equal(t, other, findBindingTagExtra(extras))

	require.Nil(t, findBindingTagExtra([][]byte{[]byte("unrelated"), nil}))
}

func TestSessionBindingDistinctTags(t *testing.T) {
	aead := testAEAD(t)

	first, err := newTLSSessionBinding(aead)
	require.NoError(t, err)

	second, err := newTLSSessionBinding(aead)
	require.NoError(t, err)

	require.NotEqual(t, first.tag, second.tag, "every control connection must get a unique binding tag")
}
