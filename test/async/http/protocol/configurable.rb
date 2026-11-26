# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/protocol/configurable"
require "async/http/protocol/http1"

describe Async::HTTP::Protocol::Configured do
	let(:underlying) {Async::HTTP::Protocol::HTTP1}
	let(:remove) {subject::REMOVE}

	# A protocol which records the exact option snapshot handed to it for every
	# client/server connection, instead of opening a real socket.
	let(:recorded) {[]}

	let(:recording_protocol) do
		records = recorded
		delegate = underlying

		Class.new do
			define_singleton_method(:names) {delegate.names}

			define_singleton_method(:client) do |peer, **options|
				records << [:client, options]
				[:client, peer, options]
			end

			define_singleton_method(:server) do |peer, **options|
				records << [:server, options]
				[:server, peer, options]
			end
		end
	end

	with "legacy flat configuration" do
		let(:configured) {subject.new(recording_protocol, timeout: 5, version: "1.1")}

		it "forwards the same flat options to both client and server" do
			configured.client(:peer)
			configured.server(:peer)

			expect(recorded).to be == [
				[:client, {timeout: 5, version: "1.1"}],
				[:server, {timeout: 5, version: "1.1"}],
			]
		end

		it "forwards protocol names from the underlying protocol" do
			expect(configured.names).to be == underlying.names
		end

		it "deep freezes the stored options" do
			expect(configured.options).to be(:frozen?)
		end

		it "exposes a frozen negotiated snapshot for each side" do
			client_snapshot = configured.client_protocol.resolve_options
			server_snapshot = configured.server_protocol.resolve_options

			expect(client_snapshot).to be(:frozen?)
			expect(server_snapshot).to be(:frozen?)
		end

		it "keeps nested containers frozen after forwarding" do
			configured = subject.new(recording_protocol, underlying => {values: [1, 2]})

			configured.client(:peer)

			# The keyword splat rebuilds the top-level hash, but nested hashes and arrays
			# handed to the underlying protocol remain frozen and independent:
			snapshot = recorded[0][1]

			expect(snapshot[underlying]).to be(:frozen?)
			expect(snapshot[underlying][:values]).to be(:frozen?)
		end
	end

	with "per-side nested overrides" do
		# The shared, nested configuration keyed by the underlying protocol class:
		let(:shared) do
			{
				underlying => {a: 1, b: 2, c: 3},
				flat: "shared",
			}
		end

		# The first endpoint explicitly removes the `:b` sub-item for clients.
		let(:client_endpoint) do
			subject.new(recording_protocol, **shared, client: {underlying => {b: remove}})
		end

		# The second endpoint explicitly keeps `:c` as nil for servers.
		let(:server_endpoint) do
			subject.new(recording_protocol, **shared, server: {underlying => {c: nil}})
		end

		it "inherits shared nested options on both sides" do
			client_endpoint.client(:peer)
			server_endpoint.server(:peer)

			expect(recorded[0]).to be == [:client, {underlying => {a: 1, c: 3}, flat: "shared"}]
			expect(recorded[1]).to be == [:server, {underlying => {a: 1, b: 2, c: nil}, flat: "shared"}]
		end

		it "removes a sub-item marked with REMOVE" do
			client_endpoint.client(:peer)

			snapshot = recorded[0][1]

			expect(snapshot[underlying]).not.to be(:key?, :b)
			expect(snapshot[underlying]).to be(:key?, :a)
			expect(snapshot[underlying]).to be(:key?, :c)
		end

		it "preserves an explicit nil instead of deleting the key" do
			server_endpoint.server(:peer)

			snapshot = recorded[0][1]

			expect(snapshot[underlying]).to be(:key?, :c)
			expect(snapshot[underlying][:c]).to be == nil
		end

		it "strips the reserved per-side keys from the negotiated snapshot" do
			client_endpoint.client(:peer)
			server_endpoint.server(:peer)

			recorded.each do |_side, options|
				expect(options).not.to be(:key?, :client)
				expect(options).not.to be(:key?, :server)
			end
		end

		it "keeps the two endpoints independent of one another" do
			client_snapshot = client_endpoint.client_protocol.options
			server_snapshot = server_endpoint.server_protocol.options

			expect(client_snapshot).not.to be_equal(server_snapshot)
			expect(client_snapshot[underlying]).not.to be(:key?, :b)
			expect(server_snapshot[underlying][:c]).to be == nil
		end
	end

	with "interleaved connections and caller mutation" do
		# The caller keeps hold of this hash and mutates it between connections.
		let(:input) do
			{
				underlying => {a: 1, b: 2, nested: {value: "original"}},
				client: {underlying => {b: remove}},
				server: {underlying => {b: nil}},
			}
		end

		let(:common) {subject.new(recording_protocol, **input)}

		it "freezes the negotiated snapshot so later mutation cannot leak in" do
			# 1. Build a client connection from the client-scoped endpoint:
			common.client(:first)

			# 2. Mutate the original input hash between the two connections:
			input[underlying][:a] = 99
			input[underlying][:nested][:value] = "mutated"
			input[:flat] = "leaked"

			# 3. Build a server connection from the server-scoped endpoint:
			common.server(:second)

			# 4. Build another connection from the common endpoint:
			common.client(:third)

			client_first = recorded[0][1]
			server_second = recorded[1][1]
			client_third = recorded[2][1]

			# Inheritance is intact, the removed key stays removed, the explicit nil
			# is preserved, and none of the later mutations are visible:
			expect(client_first).to be == {underlying => {a: 1, nested: {value: "original"}}}
			expect(server_second).to be == {underlying => {a: 1, b: nil, nested: {value: "original"}}}
			expect(client_third).to be == {underlying => {a: 1, nested: {value: "original"}}}

			# The original input hash itself is untouched by the merge machinery:
			expect(input[underlying][:a]).to be == 99
			expect(input[underlying][:b]).to be == 2
		end

		it "does not share mutable nested state with the caller" do
			snapshot = common.client_protocol.options

			expect(snapshot[underlying][:nested]).to be(:frozen?)
			expect(snapshot[underlying][:nested]).not.to be_equal(input[underlying][:nested])

			expect do
				snapshot[underlying][:nested][:value] = "frozen"
			end.to raise_exception(FrozenError)
		end
	end

	with "per-call temporary options" do
		let(:configured) do
			subject.new(
				recording_protocol,
				underlying => {a: 1, b: 2},
				client: {underlying => {b: remove}},
			)
		end

		it "applies the same merge rule to per-call overrides" do
			configured.client(:peer, underlying => {a: 9})

			snapshot = recorded[0][1]

			# The per-call override wins for `:a`, while the removed `:b` stays removed:
			expect(snapshot).to be == {underlying => {a: 9}}
		end

		it "can remove an inherited key for a single connection" do
			configured.client(:peer, underlying => {a: remove})

			snapshot = recorded[0][1]

			expect(snapshot[underlying]).not.to be(:key?, :a)
			expect(snapshot[underlying]).not.to be(:key?, :b)
		end

		it "keeps an explicit nil given for a single connection" do
			configured.client(:peer, underlying => {a: nil})

			snapshot = recorded[0][1]

			expect(snapshot[underlying]).to be(:key?, :a)
			expect(snapshot[underlying][:a]).to be == nil
		end

		it "does not let a per-call snapshot mutate the configured defaults" do
			configured.client(:peer, underlying => {a: 9})
			configured.client(:peer)

			expect(recorded[0][1]).to be == {underlying => {a: 9}}
			expect(recorded[1][1]).to be == {underlying => {a: 1}}
		end
	end

	with "deep merging" do
		it "merges nested hashes recursively instead of replacing them" do
			configured = subject.new(
				recording_protocol,
				underlying => {a: {x: 1, y: 2}},
				client: {underlying => {a: {y: 3}}},
			)

			configured.client(:peer)

			expect(recorded[0][1]).to be == {underlying => {a: {x: 1, y: 3}}}
		end
	end
end
