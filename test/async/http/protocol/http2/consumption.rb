# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/protocol/http2"
require "async/http/body/writable"
require "sus/fixtures/async/http"

describe Async::HTTP::Protocol::HTTP2 do
	include Sus::Fixtures::Async::HTTP::ServerContext
	let(:protocol) {subject}

	# Use an IP literal, as `localhost` name resolution is not available in all environments:
	def url
		"http://127.0.0.1:0"
	end

	# `reuse_port` is not supported on all platforms:
	def endpoint_options
		{protocol: protocol}
	end

	with "response consumption" do
		let(:app) do
			Protocol::HTTP::Middleware.for do |request|
				name = request.path.delete_prefix("/")
				
				body = Async::HTTP::Body::Writable.new
				response = Protocol::HTTP::Response[200, {}, body]
				
				# Mark the start of the trailer; fields added after this point are sent as trailing fields once the body finishes:
				response.headers.trailer!
				
				body.write("Hello #{name}!")
				body.write("Goodbye #{name}!")
				response.headers.add("x-digest", "digest-#{name}")
				body.close_write
				
				response
			end
		end
		
		it "settles each response independently when the application finishes consuming" do
			response_a = client.get("/a")
			response_b = client.get("/b")
			
			# The background reader receives the remaining frames for both responses, including the trailing headers, before the application consumes either body:
			Async::Task.current.with_timeout(1) do
				Async::Task.current.yield until response_a.stream.closed? and response_b.stream.closed?
			end
			
			# Receiving all frames does not settle the consumption terminal state:
			expect(response_a.consumption).not.to be(:resolved?)
			expect(response_b.consumption).not.to be(:resolved?)
			
			# The response headers continue to represent the initial headers:
			expect(response_a.headers).not.to have_keys("x-digest")
			expect(response_b.headers).not.to have_keys("x-digest")
			
			# Two observers wait for the consumption terminal state of A:
			observer_a1 = Async{response_a.consumption.wait}
			observer_a2 = Async{response_a.consumption.wait}
			
			# Cancelling the first observer neither closes the body nor affects the other stream:
			observer_a1.stop
			observer_a1.wait
			
			expect(response_a.consumption).not.to be(:resolved?)
			
			# The application reads A to the end:
			expect(response_a.read).to be == "Hello a!Goodbye a!"
			
			# The remaining observer receives the complete settlement, including A's trailing fields:
			consumption_a = observer_a2.wait
			expect(consumption_a).to be(:finished?)
			expect(consumption_a.trailers["x-digest"]).to be == ["digest-a"]
			
			# B is unaffected and still has its body available:
			expect(response_b.body.read).to be == "Hello b!"
			
			# The application closes B before consuming it fully:
			response_b.close
			
			consumption_b = response_b.consumption.wait
			expect(consumption_b).not.to be(:finished?)
			expect(consumption_b.trailers["x-digest"]).to be == ["digest-b"]
		ensure
			response_a&.close
			response_b&.close
		end
	end
end