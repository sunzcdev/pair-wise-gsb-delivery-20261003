// Copyright IBM Corp. 2016, 2025
// SPDX-License-Identifier: MPL-2.0

package plugin

const (
	// EnvUnixSocketDir specifies the directory that _plugins_ should create unix
	// sockets in. Does not affect client behavior.
	EnvUnixSocketDir = "PLUGIN_UNIX_SOCKET_DIR"

	// EnvUnixSocketGroup specifies the owning, writable group to set for Unix
	// sockets created by _plugins_. Does not affect client behavior.
	EnvUnixSocketGroup = "PLUGIN_UNIX_SOCKET_GROUP"

	envMultiplexGRPC = "PLUGIN_MULTIPLEX_GRPC"

	// envPluginVersions is set by the client when it supports per-plugin
	// version negotiation. Its value advertises the client's per-plugin
	// version catalog; its presence also tells the server that it may append
	// the negotiated per-plugin versions to the handshake response line.
	envPluginVersions = "PLUGIN_PLUGIN_VERSIONS"
)
