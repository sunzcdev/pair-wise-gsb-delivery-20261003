# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2018-2026, by Samuel Williams.
# Copyright, 2022, by Marco Concetto Rudilosso.
# Copyright, 2023, by Thomas Morgan.

require "protocol/http2/stream"

require_relative "input"
require_relative "output"

module Async
	module HTTP
		module Protocol
			module HTTP2
				# An HTTP/2 stream that manages headers, input data, and output data for a single request/response exchange.
				class Stream < ::Protocol::HTTP2::Stream
					# Initialize the stream state.
					def initialize(*)
						super
						
						@headers = nil
						
						# The application-facing header section. It is a snapshot taken when the initial headers are handed over, so protocol-side buffering of later trailer fields never mutates the view the application already received:
						@app_headers = nil
						
						@pool = nil
						
						# Input buffer, reading request body, or response body (receive_data):
						@length = nil
						@input = nil
						
						# The application can close its input before the peer finishes sending. HTTP/2 cannot close only the receiving side of a stream, so incoming data is discarded until local output also finishes. At that point, a no-error reset terminates the remaining wire stream.
						@input_closed = false
						
						# Output buffer, writing request body or response body (window_updated):
						@output = nil
					end
					
					attr_accessor :headers
					
					attr_accessor :pool
					
					attr :input
					
					# The stable, application-facing view of the initial header section.
					# @returns [Protocol::HTTP::Headers | Nil] A snapshot of the initial headers, or `nil` before any headers have been received.
					attr :app_headers
					
					# Add a header to the stream, validating against HTTP/2 constraints.
					# @parameter key [String] The header name.
					# @parameter value [String] The header value.
					def add_header(key, value, trailer: false)
						if key == CONNECTION
							raise ::Protocol::HTTP2::HeaderError, "Connection header is not allowed!"
						elsif key.start_with? ":"
							raise ::Protocol::HTTP2::HeaderError, "Invalid pseudo-header #{key}!"
						elsif key =~ /[A-Z]/
							raise ::Protocol::HTTP2::HeaderError, "Invalid upper-case characters in header #{key}!"
						else
							@headers.add(key, value, trailer: trailer)
						end
					end
					
					# Process trailing headers received after the body.
					# @parameter headers [Array] The trailing header key-value pairs.
					# @parameter end_stream [Boolean] Whether the stream ends after these headers.
					def receive_trailing_headers(headers, end_stream)
						headers.each do |key, value|
							# Mark the start of the trailer section before the first trailer field is added, so received trailers stay distinct from the initial header section. An empty trailing HEADERS frame does not create a trailer section. `trailer!` is idempotent, so trailers delivered across multiple frames are handled as well.
							@headers.trailer!
							
							# SECURITY: `trailer: true` admits only fields whose policy permits trailers, so a request-controlled trailer field cannot smuggle a field which is forbidden in that position.
							add_header(key, value, trailer: true)
						end
					end
					
					# Publish the initial header section to the application.
					#
					# The application receives a private snapshot of the initial headers while the stream keeps its own working copy. Trailer fields which arrive afterwards are buffered into the working copy and are merged into the application view once the incoming stream has ended, so a long-lived request object always sees a stable initial view and can still read its trailers by field name.
					#
					# @parameter headers [Protocol::HTTP::Headers] The initial headers received on this stream.
					# @returns [Protocol::HTTP::Headers] The stable application-facing headers.
					def publish_headers(headers)
						@app_headers = headers.dup
						
						return @app_headers
					end
					
					# Merge trailer fields which arrived after the initial headers into the application-facing header view.
					#
					# This runs at the safe point where the incoming stream has ended, after the application already holds its stable initial view. Only the trailer section of the working copy is copied across, so the initial header section the application received is never reordered or replaced.
					def merge_received_trailers
						if @headers and @headers.trailer? and @app_headers
							@app_headers.trailer!
							
							@headers.trailer.each do |key, value|
								@app_headers.add(key, value, trailer: true)
							end
						end
					end
					
					# Process an incoming HEADERS frame, dispatching to initial or trailing header handling.
					# @parameter frame [Protocol::HTTP2::HeadersFrame] The headers frame to process.
					def process_headers(frame)
						if @headers and frame.end_stream?
							self.receive_trailing_headers(super, frame.end_stream?)
						else
							self.receive_initial_headers(super, frame.end_stream?)
						end
						
						if frame.end_stream?
							# Safe point: the peer will not send anything else on this stream. Move any buffered trailer fields into the application view, then release the input side once the final body chunk has been queued.
							self.merge_received_trailers
							
							if @input
								@input.close_write
							end
						end
					rescue ::Protocol::HTTP::InvalidTrailerError => error
						Console.warn(self, error)
						
						send_reset_stream(::Protocol::HTTP2::Error::PROTOCOL_ERROR)
					rescue ::Protocol::HTTP2::HeaderError => error
						Console.debug(self, "Error while processing headers!", error)
						
						send_reset_stream(error.code)
					end
					
					# @returns [Input | Nil] The input body for this stream, if available.
					def wait_for_input
						return @input
					end
					
					# Prepare the input stream which will be used for incoming data frames.
					# @return [Input] the input body.
					def prepare_input(length)
						if @input.nil?
							@input = Input.new(self, length)
						else
							raise ArgumentError, "Input body already prepared!"
						end
					end
					
					# Update the local flow control window after receiving data.
					# @parameter frame [Protocol::HTTP2::DataFrame] The received data frame.
					def update_local_window(frame)
						consume_local_window(frame)
						
						# This is done on demand in `Input#read`:
						# request_window_update
					end
					
					# Process an incoming DATA frame and write it to the input body.
					# @parameter frame [Protocol::HTTP2::DataFrame] The data frame to process.
					# @returns [String] The unpacked data.
					def process_data(frame)
						data = frame.unpack
						
						if input = @input
							unless data.empty?
								input.write(data)
							end
							
							if frame.end_stream?
								input.close_write
							end
						else
							# The application has closed the input, so discard incoming data while maintaining flow control for the stream.
							request_window_update
						end
						
						return data
					rescue ::Protocol::HTTP2::ProtocolError
						raise
					rescue # Anything else...
						send_reset_stream(::Protocol::HTTP2::Error::INTERNAL_ERROR)
					end
					
					# Close the application-facing receiving side of the stream. While local output remains active, incoming data is discarded with flow-control updates. Once local output is also closed, the remaining wire stream is terminated without an error.
					# @parameter input [Input] The input body being closed.
					# @parameter error [Exception | Nil] The error which closed the input.
					def finish_input(input, error = nil)
						if @input.equal?(input)
							@input = nil
							@input_closed = true
							
							if error
								send_reset_stream(::Protocol::HTTP2::Error::INTERNAL_ERROR)
							else
								close_if_finished
							end
						end
					end
					
					# Set the body and begin sending it.
					def send_body(body, trailer = nil)
						@output = Output.new(self, body, trailer)
						
						@output.start
					end
					
					# Called when the output terminates normally.
					def finish_output(error = nil)
						return if self.closed?
						
						trailer = @output&.trailer
						
						@output = nil
						
						if error
							send_reset_stream(::Protocol::HTTP2::Error::INTERNAL_ERROR)
						else
							# Write trailer?
							if trailer&.any?
								send_headers(trailer, ::Protocol::HTTP2::END_STREAM)
							else
								send_data(nil, ::Protocol::HTTP2::END_STREAM)
							end
						end
					end
					
					# Called when the flow control window is updated.
					# @parameter size [Integer] The new window size.
					# @returns [Boolean] Always returns `true`.
					def window_updated(size)
						super
						
						@output&.window_updated(size)
						
						return true
					end
					
					# Send headers and apply any pending application-side closure.
					def send_headers(...)
						result = super
						close_if_finished
						return result
					end
					
					# Send data and apply any pending application-side closure.
					def send_data(...)
						result = super
						close_if_finished
						return result
					end
					
					# When the stream transitions to the closed state, this method is called. There are roughly two ways this can happen:
					# - A frame is received which causes this stream to enter the closed state. This method will be invoked from the background reader task.
					# - A frame is sent which causes this stream to enter the closed state. This method will be invoked from that task.
					# While the input stream is relatively straight forward, the output stream can trigger the second case above
					def closed(error)
						if error.is_a?(::Protocol::HTTP2::StreamError) && error.code == ::Protocol::HTTP2::Error::NO_ERROR
							error = nil
						end
						
						super
						
						if input = @input
							@input = nil
							input.close_write(error)
						end
						
						if output = @output
							@output = nil
							
							if error
								output.stop(error)
							else
								output.close_stream
							end
						end
						
						if pool = @pool and @connection
							pool.release(@connection)
						end
						
						return self
					end
					
					private
					
					# If both application-facing directions are closed but the peer has not finished, terminate the remaining wire stream without an error.
					def close_if_finished
						if @input_closed && @state == :half_closed_local
							send_reset_stream(::Protocol::HTTP2::Error::NO_ERROR)
						end
					end
				end
			end
		end
	end
end
