// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/rpc"
	"reflect"
)

const businessErrorRPCPrefix = "__go-plugin-business-error-v1:"

const maxBusinessErrorPayloadBytes = 8192

type businessErrorEnvelope struct {
	Version int                     `json:"v"`
	Root    int                     `json:"root"`
	Nodes   []businessErrorWireNode `json:"nodes"`
}

type businessErrorWireNode struct {
	Type   string         `json:"type,omitempty"`
	Text   string         `json:"text"`
	Fields map[string]any `json:"fields,omitempty"`
	Causes []int          `json:"causes,omitempty"`
}

func encodeBusinessError(err error) ([]byte, bool) {
	if err == nil {
		return nil, false
	}

	enc := businessErrorEncoder{
		seen: make(map[errorIdentity]int),
	}
	root, depth := enc.add(err, 0)
	if depth < 0 || root < 0 || len(enc.nodes) == 0 {
		return nil, false
	}

	registeredRoot := -1
	for i, node := range enc.nodes {
		if node.Type != "" {
			registeredRoot = i
			break
		}
	}
	if registeredRoot < 0 {
		return nil, false
	}

	data, err := json.Marshal(&businessErrorEnvelope{
		Version: 1,
		Root:    registeredRoot,
		Nodes:   enc.nodes,
	})
	if err != nil {
		return nil, false
	}
	if len(data) > maxBusinessErrorPayloadBytes {
		return nil, false
	}
	return data, true
}

type businessErrorEncoder struct {
	nodes []businessErrorWireNode
	seen  map[errorIdentity]int
}

type errorIdentity struct {
	typ reflect.Type
	ptr uintptr
}

func (enc *businessErrorEncoder) add(err error, depth int) (int, int) {
	if err == nil || depth > maxBusinessErrorDepth || len(enc.nodes) >= maxBusinessErrorNodes {
		return -1, -1
	}

	if key, ok := errorIdentityFor(err); ok {
		if index, ok := enc.seen[key]; ok {
			return index, depth
		}
	}

	index := len(enc.nodes)
	if key, ok := errorIdentityFor(err); ok {
		enc.seen[key] = index
	}
	enc.nodes = append(enc.nodes, businessErrorWireNode{Text: err.Error()})

	node := enc.nodes[index]
	if entry, meta := registeredBusinessErrorFor(err); entry != nil && meta != nil {
		node.Type = meta.id
		node.Fields = exportedBusinessErrorFields(entry, meta.fields)
	}

	for _, cause := range unwrapBusinessErrors(err) {
		causeIndex, causeDepth := enc.add(cause, depth+1)
		if causeIndex < 0 {
			continue
		}
		node.Causes = append(node.Causes, causeIndex)
		enc.nodes[index] = node
		if causeDepth+1 > depth {
			depth = causeDepth + 1
		}
	}

	enc.nodes[index] = node
	return index, depth
}

func errorIdentityFor(err error) (errorIdentity, bool) {
	value := reflect.ValueOf(err)
	typ := value.Type()
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return errorIdentity{}, false
		}
		if value.Kind() == reflect.Pointer {
			return errorIdentity{typ: typ, ptr: value.Pointer()}, true
		}
		value = value.Elem()
	}
	return errorIdentity{}, false
}

func unwrapBusinessErrors(err error) []error {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		return wrapped.Unwrap()
	case interface{ Unwrap() error }:
		if cause := wrapped.Unwrap(); cause != nil {
			return []error{cause}
		}
	}
	return nil
}

func exportedBusinessErrorFields(entry *registeredBusinessError, fields map[string]any) map[string]any {
	result := make(map[string]any)
	for name, value := range fields {
		if _, ok := entry.allowed[name]; ok {
			result[name] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func decodeBusinessError(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var envelope businessErrorEnvelope
	if err := decoder.Decode(&envelope); err != nil || decoder.More() {
		return nil
	}
	if envelope.Version != 1 || envelope.Root < 0 || envelope.Root >= len(envelope.Nodes) || len(envelope.Nodes) > maxBusinessErrorNodes {
		return nil
	}

	proxies := make([]*businessErrorProxy, len(envelope.Nodes))
	for i := range envelope.Nodes {
		node := envelope.Nodes[i]
		if len(node.Causes) > maxBusinessErrorNodes {
			return nil
		}
		proxies[i] = &businessErrorProxy{node: node}
	}

	root := proxies[envelope.Root]
	realizeBusinessProxy(proxies, envelope.Root, map[int]bool{})
	return root.err
}

type businessErrorProxy struct {
	node   businessErrorWireNode
	causes []error
	err    error
}

func realizeBusinessProxy(proxies []*businessErrorProxy, index int, ancestors map[int]bool) {
	p := proxies[index]
	if p.err != nil {
		return
	}

	nextAncestors := make(map[int]bool, len(ancestors)+1)
	for ancestor := range ancestors {
		nextAncestors[ancestor] = true
	}
	nextAncestors[index] = true

	entry := registeredBusinessErrorByID(p.node.Type)
	if entry != nil {
		causes := make([]error, 0, len(p.node.Causes))
		for _, causeIndex := range p.node.Causes {
			if causeIndex < 0 || causeIndex >= len(proxies) {
				continue
			}
			if nextAncestors[causeIndex] {
				causes = append(causes, businessErrorCycleReference(proxies[causeIndex].node))
				continue
			}
			realizeBusinessProxy(proxies, causeIndex, nextAncestors)
			causes = append(causes, proxies[causeIndex].err)
		}

		meta := &businessErrorMeta{
			id:      entry.id,
			message: p.node.Text,
			fields:  filterRegisteredBusinessErrorFields(entry, p.node.Fields),
			causes:  causes,
		}
		p.err = entry.make(meta)
		return
	}

	causes := make([]error, 0, len(p.node.Causes))
	for _, causeIndex := range p.node.Causes {
		if causeIndex < 0 || causeIndex >= len(proxies) {
			continue
		}
		if nextAncestors[causeIndex] {
			causes = append(causes, businessErrorCycleReference(proxies[causeIndex].node))
			continue
		}
		realizeBusinessProxy(proxies, causeIndex, nextAncestors)
		causes = append(causes, proxies[causeIndex].err)
	}

	if p.node.Type == "" {
		p.err = &remoteBasicError{message: p.node.Text, causes: causes}
		return
	}

	fields := make(map[string]any, len(p.node.Fields))
	for name, value := range p.node.Fields {
		fields[name] = value
	}
	p.err = &RemoteBusinessError{
		id:      p.node.Type,
		message: p.node.Text,
		fields:  fields,
		causes:  causes,
	}
}

func businessErrorCycleReference(node businessErrorWireNode) error {
	message := "cyclic business error reference"
	if node.Type != "" {
		message = "cyclic business error reference: " + node.Type
	}
	return &RemoteBusinessError{
		id:      node.Type,
		message: message,
	}
}

func filterRegisteredBusinessErrorFields(entry *registeredBusinessError, fields map[string]any) map[string]any {
	result := make(map[string]any)
	for name, value := range fields {
		if _, ok := entry.allowed[name]; ok {
			result[name] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// EncodeRPCError encodes a registered business error for net/rpc's string
// error channel. Other errors are returned unchanged.
func EncodeRPCError(err error) error {
	if err == nil {
		return nil
	}
	if data, ok := encodeBusinessError(err); ok {
		return errors.New(businessErrorRPCPrefix + base64.RawStdEncoding.EncodeToString(data))
	}
	return err
}

// DecodeRPCError decodes a registered business error from an rpc.ServerError.
// Local and connection errors are returned unchanged.
func DecodeRPCError(err error) error {
	var serverErr rpc.ServerError
	if !errors.As(err, &serverErr) {
		return err
	}

	text := string(serverErr)
	if len(text) < len(businessErrorRPCPrefix) || text[:len(businessErrorRPCPrefix)] != businessErrorRPCPrefix {
		return err
	}

	data, decodeErr := base64.RawStdEncoding.DecodeString(text[len(businessErrorRPCPrefix):])
	if decodeErr != nil {
		return err
	}
	if decoded := decodeBusinessError(data); decoded != nil {
		return decoded
	}
	return err
}
