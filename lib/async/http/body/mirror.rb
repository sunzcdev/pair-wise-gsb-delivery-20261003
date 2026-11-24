# frozen_string_literal: true

# Released under the MIT License.

require "async"
require "protocol/http/body/writable"

module Async
	module HTTP
		module Body
			# A bounded, non-blocking diagnostic copy of chunks which have already
			# been consumed by a primary stream.
			#
			# The mirror is deliberately a sink: the primary stream calls {#capture}
			# once for each chunk it reads.  A slow or absent observer can never apply
			# back-pressure to that call.  When the configured chunk or byte budget is
			# exhausted, additional chunks are recorded as dropped ranges instead of
			# being enqueued.
			class Mirror < ::Protocol::HTTP::Body::Writable
				DEFAULT_MAX_CHUNKS = 32
				DEFAULT_OVERFLOW = :keep_first

				Record = Struct.new(:sequence, :index, :offset, :bytesize, :chunk, keyword_init: true)
				Drop = Struct.new(:sequence, :offset, :bytesize, :reason, keyword_init: true)

				# Wrap a one-shot readable body.  The returned tap is the body the
				# primary consumer should read; its {Tap#mirror} receives copies.
				def self.tap(source, mirror: nil, **options)
					mirror ||= new(**options)
					Tap.new(source, mirror)
				end

				class << self
					alias wrap tap
					alias for_source tap
				end

				def initialize(limit_argument = nil,
					limit: nil,
					max_chunks: nil,
					max_count: nil,
					max: nil,
					chunk_limit: nil,
					count: nil,
					chunks: nil,
					capacity: nil,
					buffer: nil,
					bound: nil,
					max_bytes: nil,
					max_size: nil,
					bytes: nil,
					size: nil,
					overflow: DEFAULT_OVERFLOW,
					queue: nil,
					task: nil,
					**extra,
					&observer)
					chunk_limit = [
						max_chunks,
						max_count,
						max,
						chunk_limit,
						count,
						chunks,
						capacity,
						buffer.is_a?(Integer) ? buffer : nil,
						bound.is_a?(Integer) ? bound : nil,
						limit,
						limit_argument.is_a?(Integer) ? limit_argument : nil
					].compact.first

					byte_limit = [max_bytes, max_size, bytes, size].compact.first

					super(nil, queue: queue || Thread::Queue.new)

					@max_chunks = chunk_limit || DEFAULT_MAX_CHUNKS
					@max_bytes = byte_limit
					@overflow = overflow == :drop_newest ? :keep_first : overflow
					@extra = extra
					@task = task || Async::Task.current?

					@mutex = Mutex.new
					@records = []
					@dropped = []

					@seen_chunks = 0
					@seen_bytes = 0
					@accepted_chunks = 0
					@accepted_bytes = 0

					@accepting = true
					@paused = false
					@resume = nil
					@tasks = []

					observe(task: @task, &observer) if observer && @task
				end

				attr_reader :max_chunks, :max_bytes, :overflow
				attr_reader :records, :dropped, :tasks
				attr_reader :seen_chunks, :seen_bytes
				attr_reader :accepted_chunks, :accepted_bytes

				alias limit max_chunks

				# Preserve the original bounded queue write before replacing #write with
				# the non-blocking capture entry point.
				alias enqueue write
				private :enqueue

				# Compatibility with code which treats a mirror as a tee.
				alias events records
				alias entries records
				alias snapshots records
				alias captured_records records

				def size
					@mutex.synchronize { @records.size }
				end

				def pending_count
					@queue.size
				end

				def pending?
					!@queue.empty?
				end

				def chunks
					@mutex.synchronize { @records.map(&:chunk) }
				end
				alias captured chunks

				def bytes
					chunks.join.b
				end
				alias string bytes
				alias joined bytes
				# Do not define #to_a here: Array(mirror) in Pipe initialization would
				# otherwise coerce the mirror into its captured chunks.
				alias contents chunks
				alias buffer chunks

				def sequences
					@mutex.synchronize { @records.map(&:sequence) }
				end

				def offsets
					@mutex.synchronize { @records.map(&:offset) }
				end

				def first_sequence
					@mutex.synchronize { @records.first&.sequence }
				end

				def last_sequence
					@mutex.synchronize { @records.last&.sequence }
				end

				def start_offset
					@mutex.synchronize { @records.first&.offset }
				end

				alias offset start_offset

				def end_offset
					@mutex.synchronize do
						record = @records.last
						record ? record.offset + record.bytesize : 0
					end
				end

				def range
					@mutex.synchronize do
						first = @records.first
						last = @records.last
						next nil unless first && last

						first.sequence..last.sequence
					end
				end

				def coverage
					@mutex.synchronize do
						first = @records.first
						last = @records.last
						next nil unless first && last

						{
							chunks: first.sequence..last.sequence,
							bytes: first.offset...(last.offset + last.bytesize),
							sequences: @records.map(&:sequence),
							dropped: @dropped.map(&:sequence)
						}
					end
				end

				alias sequence_range range
				alias range_details coverage
				alias snapshot coverage

				def byte_range
					@mutex.synchronize do
						first = @records.first
						last = @records.last
						next nil unless first && last

						first.offset...(last.offset + last.bytesize)
					end
				end

				def offset_range
					byte_range
				end

				def dropped?
					@mutex.synchronize { !@dropped.empty? }
				end

				def dropped_count
					@mutex.synchronize { @dropped.size }
				end

				alias drop_count dropped_count

				def dropped_sequences
					@mutex.synchronize { @dropped.map(&:sequence) }
				end

				def dropped_ranges
					@mutex.synchronize { @dropped.map { |drop| drop.offset...(drop.offset + drop.bytesize) } }
				end

				def complete?
					@mutex.synchronize { !@accepting && !@error }
				end

				alias finished? complete?
				alias completed? complete?

				def paused?
					@mutex.synchronize { @paused }
				end

				# Pause delivery to readers.  Capture continues and remains non-blocking.
				def pause
					@mutex.synchronize { @paused = true }
					self
				end

				# Resume delivery to a blocked observer.
				def resume
					condition = nil
					@mutex.synchronize do
						@paused = false
						condition = @resume
					end
					condition&.signal
					self
				end

				# Capture one chunk from the primary read path.  This method never
				# applies back-pressure and never raises into the primary stream.
				def capture(chunk)
					return false if chunk.nil?

					@mutex.synchronize do
						return false unless @accepting

						sequence = @seen_chunks
						offset = @seen_bytes
						bytesize = chunk.bytesize

						@seen_chunks += 1
						@seen_bytes += bytesize

						reason = rejection_reason(chunk.bytesize)
						if reason
							@dropped << Drop.new(sequence: sequence, offset: offset, bytesize: bytesize, reason: reason)
							return false
						end

						copy = chunk.dup
						copy.force_encoding(chunk.encoding) if copy.encoding != chunk.encoding
						record = Record.new(
							sequence: sequence,
							index: @records.length,
							offset: offset,
							bytesize: bytesize,
							chunk: copy
						)

						@records << record
						@accepted_chunks += 1
						@accepted_bytes += bytesize

						begin
							enqueue(copy)
						rescue ::Protocol::HTTP::Body::Writable::Closed
							@accepting = false
							return false
						end

						true
					end
				rescue => error
					# Diagnostics must never break forwarding:
					@accepting = false
					@capture_error = error
					false
				end

				# Writable-style producer API.  Writing to the mirror is the same as
				# offering a captured primary chunk.
				def write(chunk)
					capture(chunk)
				end

				def read
					wait_while_paused
					chunk = super

					# If pause landed while the read was suspended in the queue, hold the
					# dequeued chunk before delivering it to the observer.
					wait_while_paused if chunk

					chunk
				end

				# Mark the primary stream finished.  Already captured chunks remain
				# available for the observer to drain.
				def close_write(error = nil)
					@mutex.synchronize { @accepting = false }
					resume
					super(error)
				end

				# An observer can close the mirror when it exits.  Future captures are
				# discarded and any observer tasks tracked by this mirror are stopped.
				def close(error = nil)
					@mutex.synchronize do
						@accepting = false
						@paused = false
					end
					resume
					stop_tasks(error)
					super(error)
				end

				# Run a transient observer task.  It is tracked so proxy/pipe shutdown
				# can reap it if the observer has not already reached EOF.
				def observe(task: Async::Task.current, transient: true, &block)
					observer = task.async(transient: transient) do |running|
						@mutex.synchronize { @tasks << running }
						begin
							each(&block)
						ensure
							@mutex.synchronize { @tasks.delete(running) }
						end
					end

					@mutex.synchronize { @tasks << observer unless @tasks.include?(observer) }
					observer
				end

				def stop_tasks(error = nil)
					tasks = @mutex.synchronize { @tasks.dup }
					current = Async::Task.current?

					tasks.each do |task|
						next if task.equal?(current) || task.finished?

						task.stop(true, cause: error)
					end

					tasks.each do |task|
						next if task.equal?(current)

						task.wait
					rescue Exception
						# Observer cancellation is best-effort cleanup.
					end
				end

				private

				def rejection_reason(bytesize)
					if @max_chunks && @accepted_chunks >= @max_chunks
						:chunk_limit
					elsif @max_bytes && @accepted_bytes + bytesize > @max_bytes
						:byte_limit
					else
						nil
					end
				end

				def wait_while_paused
					condition = nil
					@mutex.synchronize do
						break unless @paused

						condition = (@resume ||= Thread::ConditionVariable.new)
						condition.wait(@mutex) while @paused
					end
				end

				# A readable wrapper which feeds a mirror exactly once per primary read.
				class Tap < ::Protocol::HTTP::Body::Readable
					attr_reader :mirror

					def initialize(source, mirror = Mirror.new)
						@source = source
						@mirror = mirror
					end

					def read
						chunk = @source.read

						if chunk.nil?
							@mirror.close_write
						else
							@mirror.capture(chunk)
						end

						chunk
					rescue => error
						@mirror.close_write(error)
						raise
					end

					def close(error = nil)
						@mirror.close_write(error)
					ensure
						@source.close(error) if @source.respond_to?(:close)
					end

					def empty?
						@source.respond_to?(:empty?) ? @source.empty? : super
					end

					def ready?
						@source.respond_to?(:ready?) ? @source.ready? : super
					end

					def length
						@source.respond_to?(:length) ? @source.length : nil
					end
				end
			end

			# Alternative spelling for diagnostics code which uses tee terminology.
			Tee = Mirror
		end
	end
end
