// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

const jsonSubtype = "proto"

// jsonCodec pretends to be the default "proto" codec but encodes with
// encoding/json, which lets the demo send plain Go structs without generated
// protobuf messages. Business error details are unaffected (they always use
// structpb.Struct through the real proto codec path internally).
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Name() string                       { return jsonSubtype }

func init() {
	// Only the demo process registers this; it deliberately overrides the
	// proto codec for this standalone example's trivial request/response.
	encoding.RegisterCodec(jsonCodec{})
}
