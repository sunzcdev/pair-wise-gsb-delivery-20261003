# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2024, by Samuel Williams.

require "async/http/statistics"
require "async/http/mock"
require "sus/fixtures/async/http"

describe Async::HTTP::Statistics do
	include Sus::Fixtures::Async::HTTP::ServerContext
	
	let(:app) do
		Protocol::HTTP::Middleware.for do |request|
			statistics = subject.start
			
			response = Protocol::HTTP::Response[200, {}, ["Hello ", "World!"]]
			
			statistics.wrap(response) do |statistics, error|
				expect(statistics.sent).to be == 12
				expect(error).to be_nil
			end.tap do |response|
				expect(response.body).to receive(:complete_statistics)
			end
		end
	end
	
	it "client can get resource" do
		response = client.get("/")
		expect(response.read).to be == "Hello World!"
		
		expect(response).to be(:success?)
	end
	
	with "#wrap" do
		# A response which carries connection phases, exactly like the responses returned by the client:
		class PhasedResponse < Protocol::HTTP::Response
			attr_accessor :phases
		end
		
		def wrap(body, phases)
			response = PhasedResponse.new(nil, 200, Protocol::HTTP::Headers.new, Protocol::HTTP::Body::Buffered.wrap(body))
			response.phases = phases
			
			timings = nil
			subject.start.wrap(response){|timing, error| timings = timing}
			
			response.read
			response.close
			
			return timings
		end
		
		it "distinguishes a phase which did not happen from one which took no time" do
			timings = wrap(["Hello"], subject::Phases.new(pool_wait_duration: 0.0, connect_duration: nil))
			
			# The pool wait happened, but was too fast to measure:
			expect(timings.pool_wait_duration).to be == 0.0
			expect(timings).to be(:waited?)
			
			# The connection was reused, so no connection establishment happened at all:
			expect(timings.connect_duration).to be_nil
			expect(timings).not.to be(:connected?)
		end
		
		it "reports both phases when both happened" do
			timings = wrap(["Hello"], subject::Phases.new(pool_wait_duration: 0.0, connect_duration: 0.0))
			
			expect(timings.pool_wait_duration).to be == 0.0
			expect(timings.connect_duration).to be == 0.0
			expect(timings).to be(:waited?)
			expect(timings).to be(:connected?)
		end
		
		it "retains the first chunk and total duration statistics" do
			timings = wrap(["Hello"], subject::Phases.new)
			
			expect(timings.sent).to be == 5
			expect(timings.first_chunk_duration).not.to be_nil
			expect(timings.total_duration).not.to be_nil
		end
	end
	
	with "connection reuse" do
		it "reports connection establishment only for the request which performed it" do
			first = subject.start
			first_timings = nil
			first_response = client.get("/")
			first.wrap(first_response){|timing, error| first_timings = timing}
			expect(first_response.read).to be == "Hello World!"
			first_response.close
			
			# The connection is only returned to the pool once its body has been closed:
			client.pool.wait_until_free
			
			second = subject.start
			second_timings = nil
			second_response = client.get("/")
			second.wrap(second_response){|timing, error| second_timings = timing}
			expect(second_response.read).to be == "Hello World!"
			second_response.close
			
			# The first request established the connection, but did not wait for the pool:
			expect(first_timings.connect_duration).not.to be_nil
			expect(first_timings.pool_wait_duration).to be_nil
			
			# The second request reused the connection, so it did not establish one:
			expect(second_timings.connect_duration).to be_nil
			expect(second_timings.pool_wait_duration).to be_nil
		end
	end
end

# This context does not use {Sus::Fixtures::Async::HTTP::ServerContext}, because it needs a client whose pool is gated during connection establishment.
describe Async::HTTP::Statistics do
	include Sus::Fixtures::Async::ReactorContext
	
	# Wraps an endpoint so that connection establishment blocks until the gate is opened, and records when it starts and finishes.
	class GatedEndpoint
		def initialize(endpoint, started:, gate:, events:)
			@endpoint = endpoint
			@started = started
			@gate = gate
			@events = events
		end
		
		def scheme = @endpoint.scheme
		def authority = @endpoint.authority
		def protocol = @endpoint.protocol
		def path = @endpoint.path
		def secure? = false
		
		def connect
			@events << [:connect_started, Async::Clock.now]
			@started.enqueue(:started)
			@gate.dequeue
			@events << [:connect_finished, Async::Clock.now]
			
			return @endpoint.connect
		end
	end
	
	let(:events) {[]}
	# A queue (rather than a notification) so that the signal is not lost if A starts before the test begins waiting:
	let(:started) {Async::Queue.new}
	let(:gate) {Async::Queue.new}
	let(:mock_endpoint) {Async::HTTP::Mock::Endpoint.new(Async::HTTP::Protocol::HTTP1, "http", "localhost")}
	
	let(:client) do
		Async::HTTP::Client.new(
			GatedEndpoint.new(mock_endpoint, started: started, gate: gate, events: events),
			limit: 1, retries: 0
		)
	end
	
	def timed_get
		timings = nil
		response = client.get("/")
		subject.start.wrap(response){|timing, error| timings = timing}
		expect(response.read).to be == "Hello World!"
		
		return response, timings
	end
	
	it "attributes the pool wait and the connection establishment to the requests which experienced them" do
		server = reactor.async do
			mock_endpoint.run do |request|
				Protocol::HTTP::Response[200, {}, ["Hello World!"]]
			end
		end
		
		a_response = nil
		a_timings = nil
		a = reactor.async do
			a_response, a_timings = timed_get
			a_response.close
		end
		
		# Wait until A is stuck establishing the connection:
		started.dequeue
		
		b_response = nil
		b_timings = nil
		b = reactor.async do
			b_response, b_timings = timed_get
			b_response.close
		end
		
		# Let B wait for the pool slot while A is still in the handshake:
		reactor.sleep(0.01)
		expect(b_timings).to be_nil
		
		gate.enqueue(:open)
		
		a.wait
		b.wait
		
		# A established the connection and did not wait for the pool:
		expect(a_timings.connect_duration).not.to be_nil
		expect(a_timings.pool_wait_duration).to be_nil
		expect(a_timings).to be(:connected?)
		expect(a_timings).not.to be(:waited?)
		
		# B waited for the slot and then reused the same connection:
		expect(b_timings.connect_duration).to be_nil
		expect(b_timings.pool_wait_duration).not.to be_nil
		expect(b_timings).not.to be(:connected?)
		expect(b_timings).to be(:waited?)
		
		# B could only proceed once A had finished establishing the connection:
		expect(b_timings.pool_wait_duration).to be > a_timings.connect_duration
		
		# The recorded timeline matches the real one: A's connection established before B's wait finished, and B never connected:
		expect(a_timings.connect_started).to be <= a_timings.connect_finished
		expect(b_timings.pool_wait_started).to be <= b_timings.pool_wait_finished
		expect(b_timings.pool_wait_finished).to be >= a_timings.connect_finished
		expect(b_timings.connect_started).to be_nil
		expect(b_timings.connect_finished).to be_nil
		
		# Only one connection was ever established, and both requests used it:
		expect(events.count{|type, time| type == :connect_started}).to be == 1
		expect(b_response.connection).to be_equal(a_response.connection)
		
		# The original body statistics are retained for both requests:
		expect(a_timings.sent).to be == 12
		expect(b_timings.sent).to be == 12
		expect(a_timings.first_chunk_duration).not.to be_nil
		expect(b_timings.first_chunk_duration).not.to be_nil
		expect(a_timings.total_duration).not.to be_nil
		expect(b_timings.total_duration).not.to be_nil
	ensure
		gate.enqueue(:open) if gate.empty?
		a&.stop
		b&.stop
		client.close
		server&.stop
	end
end
