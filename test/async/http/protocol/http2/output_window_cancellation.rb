# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/protocol/http2"
require "async/http/body/writable"

require "sus/fixtures/async/scheduler_context"

# A local stand-in for the wire layer. It records every frame the connection
# accepts. A frame present here has been handed to the connection and has
# consumed flow-control credit; anything still inside the output task (for
# example a fragment parked on an exhausted window) never reaches this array.
class FrameOutputDouble
	def initialize
		@frames = []
	end

	attr :frames

	def write_frame(frame)
		@frames << frame

		return frame
	end

	def flush
	end

	def close
	end

	def closed?
		false
	end
end

# A connection whose framer is the local double, so no sockets or background
# reader are needed while the real flow-control windows still do their
# bookkeeping. Inside the protocol gem `Stream.create` resolves to the base
# class, so create_stream is overridden to build the async-http Stream, which
# carries the output task and window-update wiring under test.
class WindowedConnection < Async::HTTP::Protocol::HTTP2::Client
	def initialize(frame_output)
		@frame_output = frame_output

		super(nil)

		@framer = frame_output
	end

	attr :frame_output

	def create_stream(id = next_stream_id, &block)
		Async::HTTP::Protocol::HTTP2::Stream.create(self, id)
	end
end

describe Async::HTTP::Protocol::HTTP2::Output do
	include Sus::Fixtures::Async::SchedulerContext

	# Both fragments fit inside the 65,535 octet stream window, but the
	# connection window is deliberately only large enough to admit one of them.
	FIRST_FRAME_SIZE = 500
	SECOND_FRAME_SIZE = 500
	CONNECTION_WINDOW_SIZE = FIRST_FRAME_SIZE
	WINDOW_UPDATE_INCREMENT = 1000

	# Bigger than the 16,384 octet protocol maximum frame size, yet inside a
	# single default flow-control window.
	BODY_SIZE = 50_000

	let(:frame_output) {FrameOutputDouble.new}
	let(:connection) {WindowedConnection.new(frame_output)}

	let(:stream) do
		connection.create_stream.tap(&:open!)
	end

	let(:neighbour_stream) do
		connection.create_stream.tap(&:open!)
	end

	let(:body) {Async::HTTP::Body::Writable.new}

	# Shrink the connection-wide window so it admits exactly one frame.
	def shrink_connection_window!
		connection.remote_window.consume(
			::Protocol::HTTP2::Window::DEFAULT_CAPACITY - CONNECTION_WINDOW_SIZE
		)
	end

	# Attach the body through the stream so the stream owns the output task and
	# can stop it when it closes.
	def start_output(target_stream = stream, target_body = body)
		target_stream.send_body(target_body)

		output = target_stream.instance_variable_get(:@output)
		task = output.instance_variable_get(:@task)

		return output, task
	end

	def data_frames
		frame_output.frames.select do |frame|
			frame.is_a?(::Protocol::HTTP2::DataFrame) && frame.length > 0
		end
	end

	def data_frames_for(target_stream)
		data_frames.select do |frame|
			frame.stream_id == target_stream.id
		end
	end

	# A minimal connection stand-in exposing only the window surface
	# Output#write consults.
	class ConnectionWindowStub
		def initialize(available, maximum)
			@available = available
			@maximum = maximum
		end

		def available_frame_size
			[@available, @maximum].min
		end
	end

	# A minimal stream stand-in exposing only the window surface Output#write
	# consults. The two windows can be set independently to prove the output
	# waits on *both* of them, not just the stream window.
	class WindowStub
		def initialize(stream_available:, connection_available:, maximum: 16_384)
			@stream_available = stream_available
			@maximum = maximum
			@connection = ConnectionWindowStub.new(connection_available, maximum)
			@sends = []
		end

		attr :sends
		attr :connection

		def available_frame_size
			[@stream_available, @maximum].min
		end

		def send_data(chunk, **options)
			@sends << [chunk, options]
		end

		def finish_output(error = nil)
		end
	end

	with "frame-size gating" do
		it "offers no frame when the connection window is empty even if the stream window is full" do
			stub = WindowStub.new(stream_available: 65_535, connection_available: 0)
			output = subject.new(stub, body)

			Async do
				task = output.start
				body.write("x" * 100)

				# Let the task reach the wait; without the connection-window check it
				# would attempt a send instead of parking:
				3.times {Async::Task.current.yield}

				expect(stub.sends).to be(:empty?)

				task.stop
			end.wait
		end

		it "bounds the frame by the smaller of the stream and connection windows" do
			stub = WindowStub.new(stream_available: 65_535, connection_available: 100)
			output = subject.new(stub, body)

			Async do
				task = output.start
				body.write("y" * 500)

				Async::Task.current.with_timeout(1) do
					Async::Task.current.yield until stub.sends.any?
				end

				expect(stub.sends.first.first.bytesize).to be == 100

				task.stop
			ensure
				task&.stop
				body.close rescue nil
			end.wait
		end
	end

	with "cancellation while the next frame is not yet accepted" do
		it "keeps credit only for accepted frames and settles a repeated close once" do
			shrink_connection_window!

			output, task = start_output

			neighbour_body = Async::HTTP::Body::Writable.new
			_neighbour_output, neighbour_task =
				start_output(neighbour_stream, neighbour_body)

			Async do
				# Initial windows, as the contract starts here:
				expect(connection.remote_window.available).to be == CONNECTION_WINDOW_SIZE
				expect(stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY
				expect(neighbour_stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY

				body.write("a" * FIRST_FRAME_SIZE)
				body.write("b" * SECOND_FRAME_SIZE)

				# The first frame is accepted; the connection window is then empty, so
				# the second fragment stays inside the output task and is never handed
				# to the connection:
				Async::Task.current.with_timeout(1) do
					Async::Task.current.yield until data_frames.size == 1
				end

				# A couple of extra iterations prove nothing else sneaks through:
				3.times {Async::Task.current.yield}

				expect(data_frames.first.length).to be == FIRST_FRAME_SIZE

				# The accepted frame consumed credit from both windows:
				expect(connection.remote_window.available).to be == 0
				expect(stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY - FIRST_FRAME_SIZE

				# Close the stream while its output task still owns the second
				# fragment. The reset is written and the parked output task is stopped:
				stream.send_reset_stream(::Protocol::HTTP2::Error::NO_ERROR)

				Async::Task.current.with_timeout(1) do
					task.wait
				end

				expect(body).to be(:closed?)

				# Only the first DATA frame was ever accepted. The second fragment was
				# cancelled while still owned by the task, so it neither appears on the
				# wire nor consumes any credit:
				expect(data_frames.size).to be == 1
				expect(connection.remote_window.available).to be == 0
				expect(stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY - FIRST_FRAME_SIZE

				# Deliver a connection-level WINDOW_UPDATE. The closed stream has no
				# output task left, so it cannot spend this credit:
				update = ::Protocol::HTTP2::WindowUpdateFrame.new(0)
				update.pack(WINDOW_UPDATE_INCREMENT)
				connection.receive_window_update(update)

				expect(connection.remote_window.available).to be == WINDOW_UPDATE_INCREMENT

				# The same closure arriving again must not settle credit a second time
				# or emit another reset:
				frames_before = frame_output.frames.size
				stream.close!

				expect(frame_output.frames.size).to be == frames_before
				expect(connection.remote_window.available).to be == WINDOW_UPDATE_INCREMENT

				# The neighbour has a full stream window, so the replenished connection
				# window is the only limit. Offer twice the available credit:
				neighbour_body.write("c" * (WINDOW_UPDATE_INCREMENT * 2))

				Async::Task.current.with_timeout(1) do
					Async::Task.current.yield until data_frames.any? do |frame|
						frame.stream_id == neighbour_stream.id
					end
				end

				written = data_frames.last
				expect(written.stream_id).to be == neighbour_stream.id
				expect(written.length).to be == WINDOW_UPDATE_INCREMENT

				# It consumed exactly the replenished connection credit, and the same
				# bytes were charged to its stream window:
				expect(connection.remote_window.available).to be == 0
				expect(neighbour_stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY - WINDOW_UPDATE_INCREMENT

				# Full accounting: one accepted frame per stream, and a single reset.
				expect(data_frames.size).to be == 2
				expect(frame_output.frames.count do |frame|
					frame.is_a?(::Protocol::HTTP2::ResetStreamFrame)
				end).to be == 1
			ensure
				neighbour_task&.stop
				neighbour_body.close rescue nil
				body.close rescue nil
			end.wait
		end
	end

	with "a body written without cancellation" do
		let(:stream) do
			connection.create_stream.tap(&:open!)
		end

		it "writes the whole body, split into frames by the protocol frame limit" do
			output, task = start_output

			Async do
				# The default 16,384 octet maximum frame size is smaller than the
				# body, so several DATA frames are produced:
				body.write("d" * BODY_SIZE)
				body.close_write

				Async::Task.current.with_timeout(1) do
					task.wait
				end

				frames = data_frames
				expect(frames.sum(&:length)).to be == BODY_SIZE
				expect(frames.size).to be == (BODY_SIZE / 16_384.0).ceil
				expect(frames.map(&:length).max).to be <= 16_384

				# Every byte spent both stream and connection credit:
				expect(stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY - BODY_SIZE
				expect(connection.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY - BODY_SIZE

				# The task ended with an empty END_STREAM DATA frame:
				end_stream = frame_output.frames.last
				expect(end_stream).to be_a(::Protocol::HTTP2::DataFrame)
				expect(end_stream).to be(:end_stream?)
				expect(end_stream.length).to be == 0
			end.wait
		end

		it "waits for the connection window instead of exceeding it" do
			# Leave only CONNECTION_WINDOW_SIZE octets of connection credit, then let
			# a separate stream spend the rest. The stream under test keeps its full
			# stream window, so its output must park on the connection window rather
			# than exceed it:
			shrink_connection_window!

			exhausting = connection.create_stream
			exhausting.open!
			exhausting.send_data("z" * CONNECTION_WINDOW_SIZE,
				maximum_size: exhausting.available_frame_size)

			expect(connection.remote_window.available).to be == 0

			output, task = start_output

			Async do
				# The stream's own window is still full; only the connection window is
				# exhausted, which is precisely the condition the output must wait on:
				expect(stream.remote_window.available).to be > 0
				expect(connection.available_frame_size).to be <= 0

				body.write("e" * SECOND_FRAME_SIZE)

				# Give the output task time to park; no frame may be accepted for this
				# stream and none of its stream credit may be charged:
				3.times {Async::Task.current.yield}

				expect(data_frames_for(stream)).to be(:empty?)
				expect(connection.remote_window.available).to be == 0
				expect(stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY

				# Replenish only the connection window; the parked frame goes out:
				update = ::Protocol::HTTP2::WindowUpdateFrame.new(0)
				update.pack(SECOND_FRAME_SIZE)
				connection.receive_window_update(update)

				body.close_write

				Async::Task.current.with_timeout(1) do
					task.wait
				end

				frames = data_frames_for(stream)
				expect(frames.size).to be == 1
				expect(frames.first.length).to be == SECOND_FRAME_SIZE
				expect(connection.remote_window.available).to be == 0
				expect(stream.remote_window.available).to be ==
					::Protocol::HTTP2::Window::DEFAULT_CAPACITY - SECOND_FRAME_SIZE
			ensure
				body.close rescue nil
			end.wait
		end
	end
end