// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package shared

import "github.com/hashicorp/go-plugin"

type unavailableKind struct{}

func (unavailableKind) Error() string { return "temporarily unavailable" }

var (
	NotFoundError = plugin.RegisterBusinessError[error]("kv/resource-not-found", map[string]bool{
		"key": true,
	})
	TemporarilyUnavailableError = plugin.RegisterBusinessError[unavailableKind]("kv/temporarily-unavailable", map[string]bool{
		"retry_after_seconds": true,
		"upstream_token":      false,
	})
)

func NewNotFoundError(key string) error {
	return NotFoundError.New("resource not found: "+key, map[string]any{"key": key})
}

func NewTemporarilyUnavailableError(retryAfter int, token string, causes ...error) error {
	return TemporarilyUnavailableError.New("temporarily unavailable", map[string]any{
		"retry_after_seconds": retryAfter,
		"upstream_token":      token,
	}, causes...)
}

func IsNotFound(err error) bool {
	return NotFoundError.Is(err)
}

func IsTemporarilyUnavailable(err error) bool {
	return TemporarilyUnavailableError.Is(err)
}
