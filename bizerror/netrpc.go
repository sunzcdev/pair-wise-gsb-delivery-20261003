// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package bizerror

import (
	"encoding/json"
	"errors"
	"strings"
)

// net/rpc only transports the result of err.Error(); everything else is
// discarded by the standard library. The envelope therefore rides inside the
// message under an explicit, unlikely-to-collide prefix. Plain errors never
// get the prefix, so an old plugin and an old host keep working unchanged.

const (
	// netRPCMarker identifies a business error envelope over net/rpc.
	netRPCMarker = "go-plugin-bizerror-v1\x00"
)

// NetRPCEncode converts err into the error value a net/rpc server method
// should return. Registered business errors (including ones found anywhere
// in the cause chain) become a marked envelope; every other error is
// returned verbatim so ordinary RPC error text and classification are
// preserved.
//
// Usage on the plugin side:
//
//	func (s *RPCServer) Get(key string, resp *[]byte) error {
//		v, err := s.Impl.Get(key)
//		*resp = v
//		return bizerror.NetRPCEncode(err)
//	}
func NetRPCEncode(err error) error {
	return defaultRegistry.NetRPCEncode(err)
}

// NetRPCDecode is the client-side counterpart. A marked envelope is rebuilt
// through the registry; an unmarked error (old plugin, plain failure) is
// returned as a normal errors.New value exactly like native net/rpc does.
//
// Usage on the host side:
//
//	err := m.client.Call("Plugin.Get", key, &resp)
//	return bizerror.NetRPCDecode(err)
func NetRPCDecode(err error) error {
	return defaultRegistry.NetRPCDecode(err)
}

// NetRPCEncode / NetRPCDecode on a custom registry.
func (r *Registry) NetRPCEncode(err error) error {
	if err == nil {
		return nil
	}
	env, ok := encodeRoot(r, err)
	if !ok {
		return err
	}
	raw, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		return err
	}
	return errors.New(netRPCMarker + string(raw))
}

func (r *Registry) NetRPCDecode(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, netRPCMarker) {
		// Preserve classic net/rpc semantics: only the string survives.
		return errors.New(msg)
	}

	var env wireEnvelope
	if jsonErr := json.Unmarshal([]byte(strings.TrimPrefix(msg, netRPCMarker)), &env); jsonErr != nil {
		return errors.New(msg)
	}
	if rebuilt, ok := decodeRoot(r, env); ok {
		return rebuilt
	}
	return errors.New(msg)
}
