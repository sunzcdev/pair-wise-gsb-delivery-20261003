// Copyright IBM Corp. 2016, 2025
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-plugin/examples/grpc/shared"
)

// Here is a real implementation of KV that writes to a local file with
// the key name and the contents are the value of the key.
type KV struct{}

func (KV) Put(key string, value []byte) error {
	value = []byte(fmt.Sprintf("%s\n\nWritten from plugin-go-netrpc", string(value)))
	return os.WriteFile("kv_"+key, value, 0644)
}

func (KV) Get(key string) ([]byte, error) {
	value, err := os.ReadFile("kv_" + key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, shared.NewNotFoundError(key)
	}
	if err != nil {
		return nil, shared.NewTemporarilyUnavailableError(1, "net-rpc-secret", err)
	}
	return value, nil
}

func main() {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: shared.Handshake,
		Plugins: map[string]plugin.Plugin{
			"kv": &shared.KVPlugin{Impl: &KV{}},
		},
	})
}
