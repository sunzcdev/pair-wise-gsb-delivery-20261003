# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2020-2026, by Samuel Williams.

require "protocol/http/body/writable"

module Async
	module HTTP
		module Protocol
			module HTTP2
				# A writable body which requests window updates when data is read from it.
				class Input < ::Protocol::HTTP::Body::Writable
					# Initialize the input body.
					# @parameter stream [Stream] The HTTP/2 stream to read from.
					# @parameter length [Integer | Nil] The expected content length.
					def initialize(stream, length)
						super(length)

						@stream = stream
						@remaining = length
						@consumer = nil
					end

					# The consumer to notify when the application finishes reading this body, one way or another. This is a direct reference rather than via the stream, as the background reader may close the stream (and detach the stream's own references) before the application finishes reading buffered data.
					attr_accessor :consumer

					# Read the next chunk of data, requesting window updates as needed.
					# @returns [String | Nil] The next chunk, or `nil` if the body is complete.
					def read
						if chunk = super
							# If we read a chunk from the stream, we want to extend the window if required so more data will be provided.
							@stream&.request_window_update
						end

						# We track the expected length and check we got what we were expecting.
						if @remaining
							if chunk
								@remaining -= chunk.bytesize
							elsif @remaining > 0
								raise EOFError, "Expected #{self.length} bytes, #{@remaining} bytes short!"
							elsif @remaining < 0
								raise EOFError, "Expected #{self.length} bytes, #{@remaining} bytes over!"
							end
						end

						unless chunk
							# The application has read the body to the end:
							if consumer = @consumer
								@consumer = nil
								consumer.finish
							end
						end

						return chunk
					end

					# Close the application-facing input body and notify the stream that incoming data is no longer being consumed. While local output is active, the HTTP/2 stream remains open. Once output also closes, the remaining wire stream is terminated without an error.
					# @parameter error [Exception | Nil] The error that caused the input to be closed, if any.
					def close(error = nil)
						super

						# The application closed the body before reading it to the end:
						if consumer = @consumer
							@consumer = nil
							consumer.close
						end

						if stream = @stream
							@stream = nil
							stream.finish_input(self, error)
						end
					end
				end
			end
		end
	end
end
