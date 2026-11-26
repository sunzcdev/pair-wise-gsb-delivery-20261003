# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2026, by Samuel Williams.

require "protocol/http/body/wrapper"

require "async/clock"
require "async/pool/controller"

module Async
	module HTTP
		# Tracks response timing statistics including time to first byte and total duration.
		class Statistics
			# Describes the connection establishment phases which a single request actually went through.
			#
			# A phase which did not happen is represented by `nil`, which is distinct from a phase which happened but took no measurable time (`0.0`). For example, reusing an existing connection reports `connect_duration` as `nil`, while establishing a new connection reports the measured duration even if it is zero.
			#
			# The absolute monotonic timestamps of each phase are also available, so that concurrent requests can be cross-checked against a single timeline.
			class Phases
				# Initialize the phases.
				# @parameter pool_wait_duration [Float | Nil] The time spent waiting for a connection from the pool, or `nil` if the request did not wait.
				# @parameter connect_duration [Float | Nil] The time spent establishing a new connection, or `nil` if an existing connection was reused.
				# @parameter pool_wait_started [Float | Nil] The monotonic time at which the pool wait started.
				# @parameter pool_wait_finished [Float | Nil] The monotonic time at which the pool wait finished.
				# @parameter connect_started [Float | Nil] The monotonic time at which connection establishment started.
				# @parameter connect_finished [Float | Nil] The monotonic time at which connection establishment finished.
				def initialize(pool_wait_duration: nil, connect_duration: nil, pool_wait_started: nil, pool_wait_finished: nil, connect_started: nil, connect_finished: nil)
					@pool_wait_duration = pool_wait_duration
					@connect_duration = connect_duration
					
					@pool_wait_started = pool_wait_started
					@pool_wait_finished = pool_wait_finished
					@connect_started = connect_started
					@connect_finished = connect_finished
				end
				
				# @attribute [Float | Nil] The time spent waiting for a connection from the pool.
				attr :pool_wait_duration
				
				# @attribute [Float | Nil] The time spent establishing a new connection.
				attr :connect_duration
				
				# @attribute [Float | Nil] The monotonic time at which the pool wait started.
				attr :pool_wait_started
				
				# @attribute [Float | Nil] The monotonic time at which the pool wait finished.
				attr :pool_wait_finished
				
				# @attribute [Float | Nil] The monotonic time at which connection establishment started.
				attr :connect_started
				
				# @attribute [Float | Nil] The monotonic time at which connection establishment finished.
				attr :connect_finished
				
				# @returns [Boolean] Whether the request waited for a connection from the pool.
				def waited?
					!@pool_wait_duration.nil?
				end
				
				# @returns [Boolean] Whether the request established a new connection.
				def connected?
					!@connect_duration.nil?
				end
				
				# @returns [Hash] A JSON-compatible representation of the phases.
				def as_json(...)
					{
						pool_wait_duration: @pool_wait_duration,
						connect_duration: @connect_duration,
						pool_wait_started: @pool_wait_started,
						pool_wait_finished: @pool_wait_finished,
						connect_started: @connect_started,
						connect_finished: @connect_finished,
					}
				end
				
				# @returns [String] A JSON string representation of the phases.
				def to_json(...)
					as_json.to_json(...)
				end
				
				# Records the phases of a single acquisition from the connection pool.
				class Recorder
					# Initialize the recorder.
					# @parameter acquire_started [Float] The time at which the acquisition started.
					def initialize(acquire_started)
						@acquire_started = acquire_started
						@waited = false
						@connect_started = nil
						@connect_finished = nil
					end
					
					# Record that the acquisition had to wait for capacity.
					def waited!
						@waited = true
					end
					
					# @returns [Boolean] Whether the acquisition had to wait for capacity.
					def waited?
						@waited
					end
					
					# Record the start of connection establishment.
					def connecting!
						@connect_started ||= Clock.now
					end
					
					# Record the end of connection establishment.
					def connected!
						@connect_finished ||= Clock.now
					end
					
					# @parameter finished [Float] The time at which the acquisition finished.
					# @returns [Phases] The recorded phases.
					def to_phases(finished = Clock.now)
						if @connect_started
							# The pool wait ended when connection establishment began:
							pool_wait_finished = @connect_started
							connect_finished = @connect_finished || finished
						elsif @waited
							pool_wait_finished = finished
							connect_finished = nil
						end
						
						if @waited and pool_wait_finished
							pool_wait_duration = pool_wait_finished - @acquire_started
						else
							pool_wait_finished = nil
							pool_wait_duration = nil
						end
						
						if @connect_started
							connect_duration = connect_finished - @connect_started
						else
							connect_duration = nil
						end
						
						Phases.new(
							pool_wait_duration: pool_wait_duration,
							connect_duration: connect_duration,
							pool_wait_started: pool_wait_duration ? @acquire_started : nil,
							pool_wait_finished: pool_wait_finished,
							connect_started: @connect_started,
							connect_finished: connect_finished,
						)
					end
				end
			end
			
			# Start tracking statistics from the current time.
			# @returns [Statistics] A new statistics instance.
			def self.start
				self.new(Clock.now)
			end
			
			# Initialize the statistics tracker.
			# @parameter start_time [Float] The start time for measuring durations.
			def initialize(start_time)
				@start_time = start_time
			end
			
			# Wrap a response body with a statistics-collecting wrapper.
			#
			# If the response carries connection phases (see {Protocol::Response#phases}), they are attributed to this request only. A subsequent request which reuses the same connection reports no connection establishment of its own.
			#
			# @parameter response [Protocol::HTTP::Response] The response to wrap.
			# @returns [Protocol::HTTP::Response] The wrapped response.
			def wrap(response, &block)
				if response and response.body
					phases = response.phases if response.respond_to?(:phases)
					
					response.body = Body::Statistics.new(@start_time, response.body, block, phases)
				end
				
				return response
			end
		end
		
		# A connection pool which records, for each acquisition, whether it waited for capacity and whether it established a new connection.
		#
		# The phases are attached to the response returned for that acquisition (see {Protocol::Response#phases=}), so each request only reports the phases it actually went through. A request which reuses a connection does not inherit the connection establishment performed by another request.
		class TimingPool < Async::Pool::Controller
			# The fiber-local key used to associate an acquisition with the task performing it.
			RECORDER = :async_http_pool_recorder

			# Acquire a connection, recording the phases of the acquisition.
			#
			# @returns [Tuple(Object, Statistics::Phases)] The acquired resource and the phases it went through.
			def acquire_with_phases
				recorder = Statistics::Phases::Recorder.new(Clock.now)
				
				previous = Fiber[RECORDER]
				Fiber[RECORDER] = recorder
				
				begin
					resource = self.acquire
				ensure
					Fiber[RECORDER] = previous
				end
				
				return resource, recorder.to_phases
			end
			
			protected
			
			# Wait until a resource is available, recording whether we actually had to wait.
			def wait_for_resource
				until resource = available_resource
					Fiber[RECORDER]&.waited!
					
					@mutex.synchronize{@condition.wait(@mutex)}
				end
				
				# Be careful not to context switch or fail here.
				return resource
			end
			
			# @returns [Object] An existing resource in a "used" state.
			def available_resource
				# Wait for any other task which is already establishing a connection:
				if @guard.blocking?
					Fiber[RECORDER]&.waited!
				end
				
				super
			end
			
			# Create a new resource, recording the time spent establishing it for the task which requested it.
			def create_resource(...)
				recorder = Fiber[RECORDER]
				
				recorder&.connecting!
				
				resource = super
				
				recorder&.connected!
				
				return resource
			end
		end
		
		module Body
			# Invokes a callback once the body has finished reading.
			class Statistics < ::Protocol::HTTP::Body::Wrapper
				# Initialize the statistics body wrapper.
				# @parameter start_time [Float] The start time for measuring durations.
				# @parameter body [Protocol::HTTP::Body::Readable] The body to wrap.
				# @parameter callback [Proc] A callback to invoke when the body is closed.
				# @parameter phases [Phases | Nil] The connection phases attributed to this request.
				def initialize(start_time, body, callback, phases = nil)
					super(body)
					
					@sent = 0
					
					@start_time = start_time
					@first_chunk_time = nil
					@end_time = nil
					
					@phases = phases
					
					@callback = callback
				end
				
				attr :start_time
				attr :first_chunk_time
				attr :end_time
				
				attr :phases
				
				attr :sent
				
				# @returns [Float | Nil] The total duration from start to close, in seconds.
				def total_duration
					if @end_time
						@end_time - @start_time
					end
				end
				
				# @returns [Float | Nil] The duration from start until the first chunk was read, in seconds.
				def first_chunk_duration
					if @first_chunk_time
						@first_chunk_time - @start_time
					end
				end
				
				# The time this request spent waiting for a connection from the pool.
				#
				# @returns [Float | Nil] The duration in seconds, or `nil` if the request did not wait.
				def pool_wait_duration
					@phases&.pool_wait_duration
				end
				
				# The time this request spent establishing a new connection.
				#
				# @returns [Float | Nil] The duration in seconds, or `nil` if an existing connection was reused.
				def connect_duration
					@phases&.connect_duration
				end
				
				# @returns [Boolean] Whether this request waited for a connection from the pool.
				def waited?
					!self.pool_wait_duration.nil?
				end
				
				# @returns [Boolean] Whether this request established a new connection.
				def connected?
					!self.connect_duration.nil?
				end
				
				# @returns [Float | Nil] The monotonic time at which the pool wait started, if the request waited.
				def pool_wait_started
					@phases&.pool_wait_started
				end
				
				# @returns [Float | Nil] The monotonic time at which the pool wait finished, if the request waited.
				def pool_wait_finished
					@phases&.pool_wait_finished
				end
				
				# @returns [Float | Nil] The monotonic time at which connection establishment started, if the request connected.
				def connect_started
					@phases&.connect_started
				end
				
				# @returns [Float | Nil] The monotonic time at which connection establishment finished, if the request connected.
				def connect_finished
					@phases&.connect_finished
				end
				
				# Close the body and record the end time.
				def close(error = nil)
					complete_statistics(error)
					
					super
				end
				
				# Read the next chunk from the body, tracking timing and bytes sent.
				# @returns [String | Nil] The next chunk of data.
				def read
					chunk = super
					
					@first_chunk_time ||= Clock.now
					
					if chunk
						@sent += chunk.bytesize
					end
					
					return chunk
				end
				
				# @returns [String] A human-readable summary of the statistics.
				def to_s
					parts = ["sent #{@sent} bytes"]
					
					if duration = self.pool_wait_duration
						parts << "waited #{format_duration(duration)} for a connection"
					end
					
					if duration = self.connect_duration
						parts << "took #{format_duration(duration)} to connect"
					end
					
					if duration = self.total_duration
						parts << "took #{format_duration(duration)} in total"
					end
					
					if duration = self.first_chunk_duration
						parts << "took #{format_duration(duration)} until first chunk"
					end
					
					return parts.join("; ")
				end
				
				# @returns [Hash] A JSON-compatible representation of the statistics.
				def as_json(...)
					{
						sent: @sent,
						pool_wait_duration: self.pool_wait_duration,
						connect_duration: self.connect_duration,
						total_duration: self.total_duration,
						first_chunk_duration: self.first_chunk_duration,
						pool_wait_started: self.pool_wait_started,
						pool_wait_finished: self.pool_wait_finished,
						connect_started: self.connect_started,
						connect_finished: self.connect_finished,
					}
				end
				
				# @returns [String] A JSON string representation of the statistics.
				def to_json(...)
					as_json.to_json(...)
				end
				
				# @returns [String] A detailed representation including the wrapped body.
				def inspect
					"#{super} | \#<#{self.class} #{self.to_s}>"
				end
				
				private
				
				def complete_statistics(error = nil)
					@end_time = Clock.now
					
					@callback.call(self, error) if @callback
				end
				
				def format_duration(seconds)
					if seconds < 1.0
						return "#{(seconds * 1000.0).round(2)}ms"
					else
						return "#{seconds.round(1)}s"
					end
				end
			end
		end
	end
end
