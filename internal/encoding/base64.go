package encoding

import (
	"encoding/base64"
	"errors"
	"strings"

	"aidanwoods.dev/go-result/result"
)

var b64 = base64.RawURLEncoding.Strict()

// Encode Standard encoding for Paseto is URL safe base64 with no padding
func Encode(bytes []byte) string {
	return b64.EncodeToString(bytes)
}

// DecodedLen returns the exact number of bytes that an unpadded, URL-safe
// base64 string of the given encoded length will decode to.
//
// The boolean result is false if the encoded length cannot be a valid
// unpadded base64 length (encoded lengths are never one more than a multiple
// of four). All arithmetic is performed in 64 bits, so the calculation cannot
// overflow on 32 bit platforms.
func DecodedLen(encodedLen int) (int64, bool) {
	n := int64(encodedLen)
	if n < 0 || n%4 == 1 {
		return 0, false
	}

	return (n * 3) / 4, true
}

// Decode Standard decoding for Paseto is URL safe base64 with no padding
func Decode(encoded string) result.Result[[]byte] {
	// From: https://pkg.go.dev/encoding/base64#Encoding.Strict
	// Note that the input is still malleable, as new line characters (CR and LF) are still ignored.
	if strings.ContainsAny(encoded, "\n\r") {
		return result.Err[[]byte](errors.New("Input may not contain new lines"))
	}

	if b, err := b64.DecodeString(encoded); err != nil {
		return result.Err[[]byte](err)
	} else {
		return result.Ok(b)
	}
}
