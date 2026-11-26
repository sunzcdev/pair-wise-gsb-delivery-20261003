# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025-2026, by Samuel Williams.

module Async
	module HTTP
		module Protocol
			# A protocol wrapper that forwards pre-configured options to client and server creation.
			#
			# The options hash can contain:
			#
			# - Shared, possibly nested options which apply to both sides, keyed by protocol class
			#   (e.g. `Async::HTTP::Protocol::HTTP1 => {...}`) or by plain keyword.
			# - A `client:` nested hash holding overrides used only when building clients.
			# - A `server:` nested hash holding overrides used only when building servers.
			#
			# Within any nested hash, {REMOVE} deletes an inherited key while an explicit `nil` is
			# kept and forwarded. The input is copied and deep frozen up front, so later mutation of
			# the caller's hash can never affect this wrapper, connections already built from it, or
			# sibling endpoints derived from the same configuration.
			class Configured
				# Place this value under a key (at any nesting level) to delete that key while merging.
				#
				# Unlike `nil`, which is a meaningful value that must be preserved and forwarded to the
				# underlying protocol, an option whose value is {REMOVE} is omitted from the negotiated
				# snapshot entirely.
				REMOVE = Object.new

				# The reserved keys which carry per-side overrides rather than protocol options.
				CLIENT_KEY = :client
				SERVER_KEY = :server

				class << self
					# Deeply duplicate a nested configuration structure so the negotiated snapshot never
					# shares mutable state (nested hashes or arrays) with the caller's input.
					#
					# Only Hash and Array containers are copied and frozen; any other value (symbols,
					# nil, numbers, strings, protocol classes, SSL contexts, ...) is kept by reference
					# and left untouched, since freezing or duplicating such objects would change their
					# identity or break their behaviour.
					#
					# @parameter value [Object] The value to duplicate.
					# @returns [Object] An independent, deeply frozen copy of the structure.
					def deep_freeze(value)
						case value
						when Hash
							value.each_with_object({}) do |(key, sub_value), hash|
								hash[key] = deep_freeze(sub_value)
							end.freeze
						when Array
							value.map { |sub_value| deep_freeze(sub_value) }.freeze
						else
							# Scalar and opaque values are shared but not mutated by the merge machinery:
							value
						end
					end

					# Merge an override hash on top of a base hash using a single, deterministic rule:
					#
					# - Two hashes are merged recursively, key by key.
					# - An override whose value is {REMOVE} deletes the key from the result.
					# - Any other value (including an explicit `nil`) replaces the base value.
					#
					# Neither input hash is mutated; the returned hash contains deep, independent copies.
					#
					# @parameter base [Hash] The inherited configuration.
					# @parameter override [Hash] The scoped or per-call overrides.
					# @returns [Hash] The newly negotiated, deep frozen configuration.
					def merge_options(base, override)
						result = deep_freeze(base).to_h.dup

						override.each do |key, value|
							if value.equal?(REMOVE)
								result.delete(key)
							elsif value.is_a?(Hash) and result[key].is_a?(Hash)
								result[key] = merge_options(result[key], value)
							else
								result[key] = deep_freeze(value)
							end
						end

						return result.freeze
					end
				end

				# Initialize a configured wrapper with a protocol and its options.
				#
				# @parameter protocol [Protocol] The underlying protocol to configure.
				# @parameter options [Hash] Shared and per-side options to forward when creating connections.
				def initialize(protocol, **options)
					@protocol = protocol

					# Take a deep, frozen snapshot immediately so external mutation cannot leak in:
					@options = Configured.deep_freeze(options)
				end

				# @attribute [Protocol] The underlying protocol.
				attr :protocol

				# @attribute [Hash] The frozen, shared snapshot of the configured options.
				attr :options

				# Derive a wrapper whose configuration is scoped to one side of a connection.
				#
				# The shared options are inherited, the matching side-specific overrides are applied on
				# top of them using {Configured.merge_options}, and the reserved `:client`/`:server`
				# keys are stripped from the resulting snapshot.
				#
				# @parameter side [Symbol] Either {CLIENT_KEY} or {SERVER_KEY}.
				# @returns [Configured] A new, independently frozen wrapper for the given side.
				def scoped(side)
					shared = @options.except(CLIENT_KEY, SERVER_KEY)
					overrides = @options[side] || {}

					options = Configured.merge_options(shared, overrides)

					return Configured.new(@protocol, **options)
				end

				private :scoped

				# @returns [Configured] A client-scoped view of this configuration.
				def client_protocol
					scoped(CLIENT_KEY)
				end

				# @returns [Configured] A server-scoped view of this configuration.
				def server_protocol
					scoped(SERVER_KEY)
				end

				# Resolve the options for a single connection.
				#
				# Per-call options use the exact same interpretation as derivation: nested hashes merge
				# recursively, {REMOVE} deletes a key and `nil` is preserved. The result is a fresh
				# snapshot which is safe to hand to the underlying protocol and immune to later changes
				# of either this wrapper or the caller's hashes.
				#
				# @parameter options [Hash] Temporary options for this single connection.
				# @returns [Hash] The deep frozen, actually negotiated snapshot.
				def resolve_options(**options)
					if options.empty?
						# The stored snapshot is already a frozen, independent copy:
						return @options
					end

					return Configured.merge_options(@options, options)
				end

				# Create a client connection using the client-scoped configuration.
				#
				# @parameter peer [IO] The peer to communicate with.
				# @parameter options [Hash] Additional per-call options merged with the configured defaults.
				def client(peer, **options)
					resolved = client_protocol.resolve_options(**options)
					@protocol.client(peer, **resolved)
				end

				# Create a server connection using the server-scoped configuration.
				#
				# @parameter peer [IO] The peer to communicate with.
				# @parameter options [Hash] Additional per-call options merged with the configured defaults.
				def server(peer, **options)
					resolved = server_protocol.resolve_options(**options)
					@protocol.server(peer, **resolved)
				end

				# @returns [Array(String)] The protocol names from the underlying protocol.
				def names
					@protocol.names
				end
			end
			
			# Provides a `new` method that creates a {Configured} wrapper, allowing protocols to be instantiated with custom options.
			module Configurable
				# Create a new {Configured} instance wrapping this protocol with the given options.
				# @parameter options [Hash] Configuration options for client and server creation.
				# @returns [Configured] A configured protocol instance.
				def new(**options)
					Configured.new(self, **options)
				end
			end
		end
	end
end
