package statute

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGSSMessage(t *testing.T) {
	msg := NewGSSMessage(GSSMsgAuthentication, []byte("token"))
	want := append([]byte{GSSAuthVersion, GSSMsgAuthentication, 0, 5}, []byte("token")...)
	assert.Equal(t, want, msg.Bytes())

	parsed, err := ParseGSSMessage(bytes.NewReader(msg.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, GSSAuthVersion, parsed.Ver)
	assert.Equal(t, GSSMsgAuthentication, parsed.MTyp)
	assert.Equal(t, []byte("token"), parsed.Token)
}

func TestGSSMessageAbort(t *testing.T) {
	msg := NewGSSMessage(GSSMsgAbort, nil)
	assert.Equal(t, []byte{GSSAuthVersion, GSSMsgAbort, 0, 0}, msg.Bytes())

	parsed, err := ParseGSSMessage(bytes.NewReader(msg.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, GSSMsgAbort, parsed.MTyp)
	assert.Empty(t, parsed.Token)
}

func TestGSSMessageTruncated(t *testing.T) {
	msg := NewGSSMessage(GSSMsgAuthentication, []byte("token"))
	_, err := ParseGSSMessage(bytes.NewReader(msg.Bytes()[:6]))
	require.Error(t, err)
}
