# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2025, by Samuel Williams.

require "async/http/protocol/https"
require "async/http/a_protocol"

describe Async::HTTP::Protocol::HTTPS do
	let(:protocol) {subject.default}
	
	with ".default" do
		it "has a default instance" do
			expect(protocol).to be_a Async::HTTP::Protocol::HTTPS
		end

		it "supports http/1.0" do
			expect(protocol.names).to be(:include?, "http/1.0")
		end

		it "supports http/1.1" do
			expect(protocol.names).to be(:include?, "http/1.1")
		end

		it "supports h2" do
			expect(protocol.names).to be(:include?, "h2")
		end
	end

	with ".for" do
		it "constructs a negotiator with a specific range of protocols" do
			protocol = subject.for(["h2", "http/1.1"])

			expect(protocol.names).to be == ["h2", "http/1.1"]
		end

		it "rejects unknown protocol names" do
			expect do
				subject.for(["bogus"])
			end.to raise_exception(ArgumentError, message: be =~ /Unknown protocol name/)
		end
	end
	
	with "#protocol_for" do
		let(:buffer) {StringIO.new}
		
		it "can detect http/1.0" do
			stream = IO::Stream(buffer)
			expect(stream).to receive(:alpn_protocol).and_return("http/1.0")
			
			expect(protocol.protocol_for(stream)).to be == Async::HTTP::Protocol::HTTP10
		end
		
		it "it can detect http/1.1" do
			stream = IO::Stream(buffer)
			expect(stream).to receive(:alpn_protocol).and_return("http/1.1")
			
			expect(protocol.protocol_for(stream)).to be == Async::HTTP::Protocol::HTTP11
		end
		
		it "it can detect http/2" do
			stream = IO::Stream(buffer)
			expect(stream).to receive(:alpn_protocol).and_return("h2")

			expect(protocol.protocol_for(stream)).to be == Async::HTTP::Protocol::HTTP2
		end

		with "a restricted range of protocols" do
			let(:protocol) {subject.for(["h2", "http/1.1"])}

			it "matches negotiated protocols within the configured range" do
				stream = IO::Stream(buffer)
				expect(stream).to receive(:alpn_protocol).and_return("h2")

				expect(protocol.protocol_for(stream)).to be == Async::HTTP::Protocol::HTTP2
			end

			it "falls back to the last protocol when no protocol is negotiated" do
				stream = IO::Stream(buffer)
				expect(stream).to receive(:alpn_protocol).and_return(nil)

				expect(protocol.protocol_for(stream)).to be == Async::HTTP::Protocol::HTTP11
			end

			it "rejects negotiated protocols outside the configured range" do
				restricted = subject.for(["h2"])

				stream = IO::Stream(buffer)
				expect(stream).to receive(:alpn_protocol).and_return("http/1.1")

				expect do
					restricted.protocol_for(stream)
				end.to raise_exception(ArgumentError, message: be =~ /Could not determine protocol/)
			end
		end
	end
end
