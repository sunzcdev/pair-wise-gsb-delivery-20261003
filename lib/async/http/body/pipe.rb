# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2019-2026, by Samuel Williams.
# Copyright, 2020, by Bruno Sutic.

require "io/stream"
require_relative "writable"

module Async
	module HTTP
		module Body
			# A bidirectional pipe that connects an input body to an output body using a Unix socket pair.
			class Pipe
				# If the input stream is closed first, it's likely the output stream will also be closed.
				#
				# @parameter mirror [Mirror | Integer | Hash | Array | Nil] Optional
				# 	bounded diagnostic mirror(s) to receive copies of chunks read from
				# 	`input`.  Mirror delivery is non-blocking and never consumes `input`
				# 	a second time.
				def initialize(input, output = Writable.new, configuration = nil, task: Task.current, mirror: nil, mirrors: nil, **options, &observer)
					@input = input
					@output = output
					@task = task

					if configuration
						if configuration.respond_to?(:async) && !configuration.respond_to?(:capture)
							task = configuration
						else
							mirror ||= configuration
						end
					end

					mirror ||= options unless options.empty?

					@mirrors = []
					if mirrors
						mirrors.is_a?(Array) ? @mirrors.concat(mirrors) : @mirrors << mirrors
					end
					@mirrors << mirror if mirror
					@mirrors.compact!
					@mirrors.map!(&method(:coerce_mirror!))
					@mirror = @mirrors.first

					head, tail = ::Socket.pair(Socket::AF_UNIX, Socket::SOCK_STREAM)

					@head = ::IO::Stream(head)
					@tail = tail

					@reader = task.async(transient: true, &self.method(:reader))
					@writer = task.async(transient: true, &self.method(:writer))

					if observer
						@mirror ||= add_mirror(Mirror.new)
						@mirror.observe(task: task, &observer)
					end
				end

				# @returns [Mirror] The first diagnostic mirror attached to this pipe.
				def mirror(limit = :__use_existing__, **options, &block)
					@mirror ||= @mirrors.first

					if @mirror.nil?
						value = if limit.is_a?(Mirror) || limit.is_a?(Hash)
							limit
						elsif limit == :__use_existing__
							options
						else
							options.merge(max_chunks: limit)
						end

						@mirror = add_mirror(coerce_mirror!(value.empty? ? {} : value))
					end

					@mirror.observe(task: @task || Async::Task.current, &block) if block_given?
					@mirror
				end

				# @returns [Array<Mirror>] All diagnostic mirrors attached to this pipe.
				attr :mirrors

				# @returns [IO] The underlying IO object for the tail of the pipe.
				def to_io
					@tail
				end

				# Close the pipe and stop the reader and writer tasks.
				def close
					reader = @reader
					writer = @writer

					reader&.stop unless current_task?(reader)
					writer&.stop unless current_task?(writer)

					finish_mirrors

					@tail.close unless @tail.closed?

					wait_for_task(reader)
					wait_for_task(writer)

					@mirrors.each(&:stop_tasks)
				end

				# Wait for relay tasks to finish after natural disconnect.
				def wait
					wait_for_task(@reader)
					wait_for_task(@writer)
					self
				end

				private

				def add_mirror(mirror)
					@mirrors << mirror unless @mirrors.include?(mirror)
					@mirror = mirror
				end

				def current_task?(task)
					task.respond_to?(:current?) && task.current?
				end

				def wait_for_task(task)
					return if task.nil? || task.finished? || current_task?(task)

					task.wait
				rescue Exception
					# Cancellation and broken pipes are expected during close.
					nil
				end

				def coerce_mirror!(mirror)
					case mirror
					when Mirror
						mirror
					when Integer
						Mirror.new(max_chunks: mirror)
					when Hash
						Mirror.new(**mirror)
					when Array
						Mirror.new(*mirror)
					else
						if mirror.respond_to?(:capture)
							mirror
						else
							raise ArgumentError, "mirror must be an Async::HTTP::Body::Mirror or bounded options"
						end
					end
				end

				def capture_mirrors(chunk)
					@mirrors.each do |mirror|
						# Writable#mirror already captures from the primary #read.  If the
						# caller also passed that same mirror to the pipe, don't copy twice.
						next if @input.respond_to?(:mirrors) && @input.mirrors.include?(mirror)

						mirror.capture(chunk)
					rescue => error
						# A diagnostic mirror must never interrupt forwarding.
						Console.debug(self, "Mirror capture failed: #{error.message}") if defined?(Console)
					end
				end

				def finish_mirrors(error = nil)
					@mirrors.each do |mirror|
						mirror.close_write(error)
					rescue => error
						Console.debug(self, "Mirror close failed: #{error.message}") if defined?(Console)
					end
				end

				# Read from the @input stream and write to the head of the pipe.
				def reader(task)
					@reader = task

					task.annotate "#{self.class} reader."

					error = nil
					while chunk = @input.read
						# Copy the chunk at the single point where the one-shot input has
						# already been read.  No second read loop is started.
						capture_mirrors(chunk)
						@head.write(chunk)
						@head.flush
					end

					@head.close_write
					finish_mirrors
				rescue => error
					finish_mirrors(error)
				ensure
					@input.close(error)

					close_head if @writer&.finished?
				end
				
				# Read from the head of the pipe and write to the @output stream.
				# A write-side close on @tail produces EOF and closes @output independently of the input direction.
				def writer(task)
					@writer = task
					
					task.annotate "#{self.class} writer."
					
					while chunk = @head.read_partial
						@output.write(chunk)
					end
				rescue => error
				ensure
					@output.close_write(error)
					
					close_head if @reader&.finished?
				end
				
				def close_head
					@head.close
					
					# Both tasks are done, don't keep references:
					@reader = nil
					@writer = nil
				end
			end
		end
	end
end
