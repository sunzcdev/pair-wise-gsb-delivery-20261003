# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2026, by Samuel Williams.

require "protocol/http/body/wrapper"

require "async/clock"

module Async
	module HTTP
		# Tracks response timing statistics including time to first byte and total duration.
		class Statistics
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
			# @parameter response [Protocol::HTTP::Response] The response to wrap.
			# @returns [Protocol::HTTP::Response] The wrapped response.
			def wrap(response, &block)
				if response and response.body
					response.body = Body::Statistics.new(@start_time, response.body, block)
				end
				
				return response
			end
		end
		
		module Body
			# Invokes a callback once the body has finished reading.
			class Statistics < ::Protocol::HTTP::Body::Wrapper
				# An independent view over the statistics of a wrapped body.
				#
				# An observer accumulates its own timing and byte statistics from the moment it is registered. It is settled exactly once, either when the response terminates (end-of-file, an explicit close, or a read failure) or when it is removed before that point.
				class Observer
					# The observer was removed before the response terminated.
					REMOVED = :removed

					# The response reached a genuine end-of-file.
					EOF = :eof

					# The caller closed the response before it reached end-of-file.
					CLOSED = :closed

					# Reading from the underlying body failed.
					FAILED = :failed

					# A chunk was delivered to the observer.
					CHUNK = :chunk

					# Initialize the observer.
					# @parameter start_time [Float] The start time used for measuring durations.
					# @parameter callback [Proc] Invoked with `(observer, event, value)` for every event.
					def initialize(start_time, callback)
						@start_time = start_time
						@callback = callback

						@sent = 0
						@first_chunk_time = nil
						@end_time = nil

						@reason = nil
						@error = nil
					end

					# @returns [Float] The start time used for measuring durations.
					attr :start_time

					# @returns [Integer] The number of bytes delivered while this observer was registered.
					attr :sent

					# @returns [Float | Nil] The time at which the first chunk was delivered to this observer.
					attr :first_chunk_time

					# @returns [Float | Nil] The time at which this observer settled.
					attr :end_time

					# @returns [Symbol | Nil] Why the observer settled: one of {REMOVED}, {EOF}, {CLOSED} or {FAILED}.
					attr :reason

					# @returns [Exception | Nil] The error that terminated the response, if any.
					attr :error

					# @returns [Boolean] Whether the observer has settled and will no longer receive chunks or callbacks.
					def settled?
						!@reason.nil?
					end

					# @returns [Float | Nil] The total duration from start until this observer settled, in seconds.
					def total_duration
						if @end_time
							@end_time - @start_time
						end
					end

					# @returns [Float | Nil] The duration from start until the first chunk was delivered to this observer, in seconds.
					def first_chunk_duration
						if @first_chunk_time
							@first_chunk_time - @start_time
						end
					end

					# Record a delivered chunk and notify the callback with a {CHUNK} event.
					#
					# @parameter chunk [String] The chunk that was read.
					# @returns [Observer] The observer itself, so it can remove itself from within the callback.
					def notify_chunk(chunk)
						@first_chunk_time ||= Clock.now
						@sent += chunk.bytesize

						@callback.call(self, CHUNK, chunk)

						return self
					end

					# Settle the observer exactly once.
					#
					# The callback is invoked with the terminal reason as the event and the error (if any) as the value.
					#
					# @parameter reason [Symbol] One of {REMOVED}, {EOF}, {CLOSED} or {FAILED}.
					# @parameter error [Exception | Nil] The error that terminated the response, if any.
					# @returns [Boolean] Whether the observer was settled by this call.
					def settle(reason, error = nil)
						return false if settled?

						@reason = reason
						@error = error
						@end_time = Clock.now

						@callback.call(self, reason, error)

						return true
					end

					# @returns [String] A human-readable summary of the observer's statistics.
					def to_s
						parts = ["sent #{@sent} bytes"]

						if duration = self.total_duration
							parts << "took #{format_duration(duration)} in total"
						end

						if duration = self.first_chunk_duration
							parts << "took #{format_duration(duration)} until first chunk"
						end

						if @reason
							parts << "settled with #{@reason}"
						end

						return parts.join("; ")
					end

					private

					def format_duration(seconds)
						if seconds < 1.0
							return "#{(seconds * 1000.0).round(2)}ms"
						else
							return "#{seconds.round(1)}s"
						end
					end
				end

				# Initialize the statistics body wrapper.
				# @parameter start_time [Float] The start time for measuring durations.
				# @parameter body [Protocol::HTTP::Body::Readable] The body to wrap.
				# @parameter callback [Proc] A callback to invoke when the body is closed.
				def initialize(start_time, body, callback)
					super(body)

					@sent = 0

					@start_time = start_time
					@first_chunk_time = nil
					@end_time = nil

					@callback = callback

					@observers = []
					@closed = false
					@eof = false
					@read_error = nil
				end

				attr :start_time
				attr :first_chunk_time
				attr :end_time

				attr :sent

				# Register an observer on this body.
				#
				# The callback is invoked with `(observer, event, value)` for every event:
				#
				# - {Observer::CHUNK} with the delivered chunk as the value, for every chunk read while the observer is registered.
				# - One of the terminal events {Observer::EOF}, {Observer::CLOSED}, {Observer::FAILED} or {Observer::REMOVED}, with the error (if any) as the value. The terminal event is delivered exactly once.
				#
				# An observer may remove itself from within its own callback, for example after the first chunk.
				#
				# @yields {|observer, event, value| ...} Handles each delivered event.
				# 	@parameter observer [Observer] The observer receiving the event.
				# 	@parameter event [Symbol] The event kind.
				# 	@parameter value [String | Exception | Nil] The chunk, error, or `nil`.
				# @returns [Observer] The registered observer.
				def observe(&callback)
					observer = Observer.new(@start_time, callback)

					if @closed
						# The response has already terminated, so settle the observer immediately using the recorded terminal state:
						observer.settle(@terminal_reason, @terminal_error)
					else
						@observers << observer
					end

					return observer
				end

				# Remove an observer from the body.
				#
				# Removing an observer settles it at the current boundary but does not close the body and does not settle any other observer. Removing an observer that has already settled is a no-op, so there are no additional callbacks after the response has closed.
				#
				# @parameter observer [Observer] The observer to remove.
				# @returns [Observer | Nil] The removed observer, or `nil` if it was not registered.
				def remove(observer)
					return nil unless @observers.delete(observer)

					observer.settle(Observer::REMOVED)

					return observer
				end
				
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
				
				# Close the body and record the end time.
				#
				# Remaining observers are settled with the response's terminal reason: a read failure is reported as {Observer::FAILED} with the error, a genuine end-of-file as {Observer::EOF}, and an explicit close before end-of-file as {Observer::CLOSED}. Closing is idempotent.
				def close(error = nil)
					return if @closed

					@closed = true

					if error
						@terminal_reason = Observer::FAILED
						@terminal_error = error
					elsif @read_error
						@terminal_reason = Observer::FAILED
						@terminal_error = @read_error
					elsif @eof
						@terminal_reason = Observer::EOF
						@terminal_error = nil
					else
						@terminal_reason = Observer::CLOSED
						@terminal_error = nil
					end

					complete_statistics(error)

					# Take a snapshot: settling an observer may run arbitrary code, including removing other observers:
					observers = @observers.dup
					@observers.clear

					observers.each do |observer|
						observer.settle(@terminal_reason, @terminal_error)
					end

					super
				end

				# Read the next chunk from the body, tracking timing and bytes sent.
				# @returns [String | Nil] The next chunk of data.
				def read
					chunk = super

					@first_chunk_time ||= Clock.now

					if chunk.nil?
						# A `nil` chunk signals a genuine end-of-file:
						@eof = true
					else
						@sent += chunk.bytesize

						# Take a snapshot: a chunk callback may remove its own or another observer:
						@observers.dup.each do |observer|
							observer.notify_chunk(chunk) unless observer.settled?
						end
					end

					return chunk
				rescue => error
					# Remember the failure so an implicit or argument-less close still reports it:
					@read_error = error
					raise
				end
				
				# @returns [String] A human-readable summary of the statistics.
				def to_s
					parts = ["sent #{@sent} bytes"]
					
					if duration = self.total_duration
						parts << "took #{format_duration(duration)} in total"
					end
					
					if duration = self.first_chunk_duration
						parts << "took #{format_duration(duration)} until first chunk"
					end
					
					return parts.join("; ")
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
