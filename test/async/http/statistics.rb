# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2024, by Samuel Williams.

require "async/http/statistics"
require "sus/fixtures/async/http"

# A readable body that raises a specific error when a given chunk is requested.
class FailingBody < Protocol::HTTP::Body::Readable
	def initialize(chunks, fail_at:, error:)
		@chunks = chunks
		@index = 0
		@fail_at = fail_at
		@error = error
	end

	def read
		if @index == @fail_at
			raise @error
		end

		chunk = @chunks[@index]
		@index += 1

		return chunk
	end
end

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
end

describe Async::HTTP::Body::Statistics do
	let(:start_time) {Async::Clock.now}
	let(:chunks) {["Hello ", "World!"]}

	# A two-chunk response wrapped with a statistics collector.
	let(:body) do
		subject.new(start_time, Protocol::HTTP::Body::Buffered.new(chunks), nil)
	end

	with "a wrapped response" do
		it "supports both the legacy wrap callback and additional observers" do
			statistics = Async::HTTP::Statistics.start

			response = Protocol::HTTP::Response[200, {}, chunks]
			statistics.wrap(response) do |wrapper, error|
				expect(wrapper.sent).to be == 12
				expect(error).to be_nil
			end

			wrapper = response.body
			expect(wrapper).to be_a(subject)
			expect(wrapper).to receive(:complete_statistics)

			observer_events = []
			observer = wrapper.observe do |observer, event, value|
				observer_events << [event, value]
			end

			# Reading the whole response joins the wrapped chunks without closing it:
			expect(response.read).to be == "Hello World!"
			wrapper.close

			# The legacy callback and the new observer both see the full body:
			expect(observer.sent).to be == 12
			expect(observer.reason).to be == subject::Observer::EOF
			expect(observer_events.map(&:first)).to be == [subject::Observer::CHUNK, subject::Observer::CHUNK, subject::Observer::EOF]
		end
	end

	with "long-lived and short-lived observers" do
		it "delivers chunks up to the removal boundary and settles every observer once at EOF" do
			short_chunks = []
			short_events = []
			long_chunks = []
			long_events = []

			short_observer = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					# A short-lived observer wants to stop after the first chunk:
					short_chunks << value
					body.remove(observer)
				else
					short_events << [event, value]

					expect(observer.sent).to be == "Hello ".bytesize
					expect(value).to be_nil
				end
			end

			long_observer = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					long_chunks << value
				else
					long_events << [event, value]

					expect(observer.sent).to be == "Hello World!".bytesize
					expect(value).to be_nil
				end
			end

			# Read everything in one go:
			expect(body.join).to be == "Hello World!"
			body.close

			expect(short_chunks).to be == ["Hello "]
			expect(long_chunks).to be == ["Hello ", "World!"]

			# The short observer only ever saw the first chunk:
			expect(short_observer.sent).to be == "Hello ".bytesize
			expect(short_observer).to be(:settled?)
			expect(short_observer.reason).to be == Async::HTTP::Body::Statistics::Observer::REMOVED

			# The long observer retained the full byte count and the real termination reason:
			expect(long_observer.sent).to be == "Hello World!".bytesize
			expect(long_observer).to be(:settled?)
			expect(long_observer.reason).to be == Async::HTTP::Body::Statistics::Observer::EOF
			expect(long_observer.error).to be_nil

			# Each observer settles exactly once:
			expect(short_events).to be == [[Async::HTTP::Body::Statistics::Observer::REMOVED, nil]]
			expect(long_events).to be == [[Async::HTTP::Body::Statistics::Observer::EOF, nil]]

			# Removing an already-settled observer produces no additional callback:
			expect(body.remove(short_observer)).to be_nil
			expect(body.remove(long_observer)).to be_nil
			expect(short_events.size).to be == 1
			expect(long_events.size).to be == 1
		end
	end

	with "a failing underlying body" do
		let(:error) {RuntimeError.new("boom")}

		let(:body) do
			subject.new(start_time, FailingBody.new(chunks, fail_at: 1, error: error), nil)
		end

		it "keeps reading alive after a short observer leaves and reports the failure to the long observer" do
			short_chunks = []
			short_events = []
			long_chunks = []
			long_events = []

			short_observer = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					short_chunks << value
					body.remove(observer)
				else
					short_events << [event, value]

					expect(observer.sent).to be == "Hello ".bytesize
				end
			end

			long_observer = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					long_chunks << value
				else
					long_events << [event, value]

					expect(observer.sent).to be == "Hello ".bytesize
				end
			end

			# The first chunk is delivered to both observers and the short observer removes itself:
			expect(body.read).to be == "Hello "

			# The second read raises from the underlying body:
			expect do
				body.read
			end.to raise_exception(RuntimeError, message: be =~ /boom/)

			# Closing after the failure must not raise again, but settles the long observer with the error:
			body.close

			expect(short_chunks).to be == ["Hello "]
			expect(long_chunks).to be == ["Hello "]

			expect(short_observer.reason).to be == Async::HTTP::Body::Statistics::Observer::REMOVED
			expect(short_observer.error).to be_nil

			expect(long_observer.sent).to be == "Hello ".bytesize
			expect(long_observer.reason).to be == Async::HTTP::Body::Statistics::Observer::FAILED
			expect(long_observer.error).to be == error

			expect(short_events).to be == [[Async::HTTP::Body::Statistics::Observer::REMOVED, nil]]
			expect(long_events).to be == [[Async::HTTP::Body::Statistics::Observer::FAILED, error]]

			# Closing is idempotent and removal after close changes nothing:
			body.close
			body.close(error)
			expect(body.remove(long_observer)).to be_nil
			expect(long_events.size).to be == 1
		end
	end

	with "an explicit close before end-of-file" do
		it "reports a caller-driven close without settling removed observers again" do
			events = Hash.new {|hash, key| hash[key] = []}

			short_observer = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					body.remove(observer)
				else
					events[:short] << [event, value]
				end
			end

			long_observer = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					# Keep observing.
				else
					events[:long] << [event, value]

					expect(observer.sent).to be == "Hello ".bytesize
				end
			end

			expect(body.read).to be == "Hello "

			# The caller abandons the remaining response before end-of-file:
			body.close

			expect(short_observer.reason).to be == Async::HTTP::Body::Statistics::Observer::REMOVED
			expect(long_observer.reason).to be == Async::HTTP::Body::Statistics::Observer::CLOSED
			expect(long_observer.error).to be_nil

			expect(events[:short]).to be == [[Async::HTTP::Body::Statistics::Observer::REMOVED, nil]]
			expect(events[:long]).to be == [[Async::HTTP::Body::Statistics::Observer::CLOSED, nil]]

			expect(body.remove(short_observer)).to be_nil
			expect(body.remove(long_observer)).to be_nil
			expect(events.values.sum(&:size)).to be == 2
		end
	end

	with "an observer registered after the response closed" do
		it "settles immediately with the terminal reason without extra callbacks later" do
			events = []

			body.join
			body.close

			observer = body.observe do |observer, event, value|
				events << [event, value]
			end

			expect(observer).to be(:settled?)
			expect(observer.reason).to be == Async::HTTP::Body::Statistics::Observer::EOF
			expect(observer.sent).to be == 0
			expect(events).to be == [[Async::HTTP::Body::Statistics::Observer::EOF, nil]]

			body.close
			expect(body.remove(observer)).to be_nil
			expect(events.size).to be == 1
		end
	end

	with "an observer removed by another observer's callback" do
		it "does not deliver further chunks or a second settlement to the removed observer" do
			victim_chunks = []
			victim_events = []

			victim = body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					victim_chunks << value
				else
					victim_events << [event, value]
				end
			end

			# A second observer removes the victim from within its own chunk callback:
			body.observe do |observer, event, value|
				case event
				when Async::HTTP::Body::Statistics::Observer::CHUNK
					body.remove(victim)
				end
			end

			expect(body.read).to be == "Hello "
			expect(body.read).to be == "World!"
			body.close

			# The victim was removed during the first chunk, so it never saw the second chunk:
			expect(victim_chunks).to be == ["Hello "]
			expect(victim.sent).to be == "Hello ".bytesize
			expect(victim.reason).to be == Async::HTTP::Body::Statistics::Observer::REMOVED
			expect(victim_events).to be == [[Async::HTTP::Body::Statistics::Observer::REMOVED, nil]]
		end
	end

	with "legacy wrap compatibility" do
		it "still invokes the single wrap callback on close" do
			callback_calls = []

			wrapped = subject.new(start_time, Protocol::HTTP::Body::Buffered.new(chunks), proc do |statistics, error|
				callback_calls << [statistics.sent, error]
			end)

			expect(wrapped.join).to be == "Hello World!"
			wrapped.close

			expect(callback_calls).to be == [[12, nil]]
			expect(wrapped.sent).to be == 12
			expect(wrapped.first_chunk_time).not.to be_nil
			expect(wrapped.end_time).not.to be_nil

			# A second close does not invoke the legacy callback again:
			wrapped.close
			expect(callback_calls.size).to be == 1
		end
	end
end
