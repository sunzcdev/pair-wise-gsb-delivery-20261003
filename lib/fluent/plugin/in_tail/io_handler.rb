#
# Fluentd
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.
#

require 'fluent/plugin/input'
require 'fluent/clock'
require 'fluent/file_wrapper'

module Fluent::Plugin
  class TailInput < Fluent::Plugin::Input
    # ByteRange carries the half-open [start, end) byte offsets, in the file,
    # of the raw bytes that a line (or a record merged from several lines) was
    # read from. Offsets are computed before encoding conversion, so splitting a
    # multibyte character or a CRLF across read buffers does not affect them.
    # The ranges are attached to the line strings as instance variables, which
    # keeps the existing line-processing interfaces (including the worker mode)
    # unchanged, and are only attached when `range_key` is configured.
    module ByteRange
      INSTANCE_VARIABLE = :@fluent_in_tail_byte_range

      module_function

      # Attaches an absolute [start, end) byte range to a copy of the line and
      # returns it. A copy is used because lines sliced out of the frozen FIFO
      # buffer may be frozen, and the ranges of a multiline buffer have to be
      # mutable while further lines are appended to it.
      def attach(line, start_byte, end_byte)
        marked = line.dup
        marked.instance_variable_set(INSTANCE_VARIABLE, [start_byte, end_byte])
        marked
      end

      def of(line)
        line.instance_variable_get(INSTANCE_VARIABLE)
      end

      # Extends the range already attached to a buffer up to the end of the
      # range of another line when the latter is appended to the buffer. When
      # the buffer does not carry a range yet (e.g. it was just reset to an
      # empty string), the range of the appended line is adopted.
      def merge!(buffer, line)
        line_range = of(line)
        return buffer unless line_range

        buffer_range = of(buffer)
        if buffer_range
          buffer_range[1] = line_range[1]
        else
          # Use a fresh array instead of the one attached to the line: the line
          # may be shared with a worker batch, and mutating its range would
          # corrupt the range of the other copy.
          buffer.instance_variable_set(INSTANCE_VARIABLE, [line_range[0], line_range[1]])
        end
        buffer
      end

      # Builds the value stored in the record field named by `range_key` from
      # the absolute [start, end) byte range attached to the line.
      def to_record_value(range)
        start_byte, end_byte = range
        {
          'start' => start_byte,
          'end' => end_byte,
          'size' => end_byte - start_byte,
        }
      end
    end

    class TailWatcher
      class FIFO
        def initialize(encoding, log, max_line_size=nil, encoding_to_convert=nil, track_byte_range: false)
          @buffer = ''.force_encoding(encoding)
          @eol = "\n".encode(encoding).freeze
          @encoding_to_convert = encoding_to_convert
          @max_line_size = max_line_size
          @skip_current_line = false
          @skipping_current_line_bytesize = 0
          @log = log
          @track_byte_range = track_byte_range
          # Total bytes appended to and consumed (sliced out) from the FIFO.
          # The difference equals the raw bytesize of the data left in @buffer.
          @total_appended_bytesize = 0
          @total_consumed_bytesize = 0
          # File offset corresponding to the first byte the FIFO has received.
          # It is lazily fixed on the first read of the opened IO.
          @base_byte = nil
        end

        attr_reader :buffer, :max_line_size

        def <<(chunk)
          @buffer << chunk
          @total_appended_bytesize += chunk.bytesize if @track_byte_range
        end

        # Fixes the file offset of the first received byte from the current IO
        # position. Called after every read, but only the first call for a FIFO
        # instance sets the base; later reads continue from the same origin.
        def sync_base(io_pos)
          return unless @track_byte_range && @base_byte.nil?

          @base_byte = io_pos - @total_appended_bytesize
        end

        def convert(s)
          if @encoding_to_convert
            s.encode!(@encoding_to_convert)
          else
            s
          end
        rescue
          s.encode!(@encoding_to_convert, :invalid => :replace, :undef => :replace)
        end

        def read_lines(lines)
          idx = @buffer.index(@eol)
          has_skipped_line = false

          until idx.nil?
            # Using freeze and slice is faster than slice!
            # See https://github.com/fluent/fluentd/pull/2527
            @buffer.freeze
            slice_position = idx + 1
            rbuf = @buffer.slice(0, slice_position)
            @buffer = @buffer.slice(slice_position, @buffer.size - slice_position)
            # The raw bytes of the line occupied these absolute offsets in the
            # file, before any encoding conversion. A CRLF line is covered up to
            # and including the line feed.
            if @track_byte_range
              line_start = @base_byte + @total_consumed_bytesize
              line_end = line_start + rbuf.bytesize
              @total_consumed_bytesize += rbuf.bytesize
            end
            idx = @buffer.index(@eol)

            is_long_line = @max_line_size && (
              @skip_current_line || rbuf.bytesize > @max_line_size
            )

            if is_long_line
              @log.warn "received line length is longer than #{@max_line_size}"
              if @skip_current_line
                @log.debug("The continuing line is finished. Finally discarded data: ") { convert(rbuf).chomp }
              else
                @log.debug("skipped line: ") { convert(rbuf).chomp }
              end
              has_skipped_line = true
              @skip_current_line = false
              @skipping_current_line_bytesize = 0
              next
            end

            converted = convert(rbuf)
            lines << if @track_byte_range
                       ByteRange.attach(converted, line_start, line_end)
                     else
                       converted
                     end
          end

          is_long_current_line = @max_line_size && (
            @skip_current_line || @buffer.bytesize > @max_line_size
          )

          if is_long_current_line
            @log.debug(
              "The continuing current line length is longer than #{@max_line_size}." +
              " The received data will be discarded until this line is finished." +
              " Discarded data: "
            ) { convert(@buffer).chomp }
            @skip_current_line = true
            @skipping_current_line_bytesize += @buffer.bytesize
            if @track_byte_range
              # Keep the offsets of future lines stable by counting the bytes
              # which are discarded from the buffer as consumed.
              @total_consumed_bytesize += @buffer.bytesize
            end
            @buffer.clear
          end

          return has_skipped_line
        end

        def reading_bytesize
          return @skipping_current_line_bytesize if @skip_current_line
          @buffer.bytesize
        end
      end

      class IOHandler
        BYTES_TO_READ = 64 * 1024
        SHUTDOWN_TIMEOUT = 5
        ASYNC_PENDING = :async_pending

        attr_accessor :shutdown_timeout

        def initialize(watcher, path:, read_lines_limit:, read_bytes_limit_per_second:, max_line_size: nil, log:, open_on_every_update:, encoding: Encoding::ASCII_8BIT, encoding_to_convert: nil, range_key: nil, metrics:, &receive_lines)
          @watcher = watcher
          @path = path
          @read_lines_limit = read_lines_limit
          @read_bytes_limit_per_second = read_bytes_limit_per_second
          @receive_lines = receive_lines
          @open_on_every_update = open_on_every_update
          @encoding = encoding
          @fifo = FIFO.new(encoding, log, max_line_size, encoding_to_convert, track_byte_range: !range_key.nil?)
          @lines = []
          @io = nil
          @notify_mutex = Mutex.new
          @log = log
          @start_reading_time = nil
          @number_bytes_read = 0
          @shutdown_start_time = nil
          @shutdown_timeout = SHUTDOWN_TIMEOUT
          @shutdown_mutex = Mutex.new
          @eof = false
          @metrics = metrics
          @async_pending = false
          @notify_pending = false
          @pending_position = nil
          @pending_read_more = false

          @log.info "following tail of #{@path}"
        end

        def group_watcher
          @watcher.group_watcher
        end

        def on_notify
          @notify_mutex.synchronize { handle_notify }
        end

        def ready_to_shutdown(shutdown_start_time = nil)
          @shutdown_mutex.synchronize {
            @shutdown_start_time =
              shutdown_start_time || Fluent::Clock.now
          }
        end

        def close
          if @io && !@io.closed?
            @io.close
            @io = nil
            @metrics.closed.inc
          end
        end

        def opened?
          !!@io
        end

        def eof?
          @eof
        end

        def drained?
          @eof && @lines.empty? && !@async_pending
        end

        def pending?
          @async_pending
        end

        # Completes a receive callback that returned ASYNC_PENDING. The caller
        # must invoke TailWatcher#on_notify when this returns true.
        def complete_async(success)
          @notify_mutex.synchronize do
            raise 'no asynchronous receive is pending' unless @async_pending

            begin
              if success
                @watcher.pe.update_pos(@pending_position)
                @lines.clear
              end
              should_notify = (success && @pending_read_more) || @notify_pending
              @pending_read_more = false if success
              should_notify
            rescue => error
              @log.error error.to_s
              @log.error_backtrace
              @notify_pending
            ensure
              @async_pending = false
              @notify_pending = false
              @pending_position = nil
            end
          end
        end

        private

        def bytes_to_read
          BYTES_TO_READ
        end

        def limit_bytes_per_second_reached?
          return false if @read_bytes_limit_per_second < 0 # not enabled by conf
          return false if @number_bytes_read < @read_bytes_limit_per_second

          @start_reading_time ||= Fluent::Clock.now
          time_spent_reading = Fluent::Clock.now - @start_reading_time
          @log.debug("time_spent_reading: #{time_spent_reading} #{ @watcher.path}")

          if time_spent_reading < 1
            true
          else
            @start_reading_time = nil
            @number_bytes_read = 0
            false
          end
        end

        def should_shutdown_now?
          # Ensure to read all remaining lines, but abort immediately if it
          # seems to take too long time.
          @shutdown_mutex.synchronize {
            return false if @shutdown_start_time.nil?
            return Fluent::Clock.now - @shutdown_start_time > @shutdown_timeout
          }
        end

        def handle_notify
          if @async_pending
            @notify_pending = true
            return
          end

          # A notification may arrive while a rejected batch is waiting for capacity.
          @pending_read_more = true unless @lines.empty?

          if limit_bytes_per_second_reached? || group_watcher&.limit_lines_reached?(@path)
            @metrics.throttled.inc
            return
          end

          with_io do |io|
            iobuf = ''.force_encoding(@encoding)
            begin
              read_more = false
              has_skipped_line = false

              if !io.nil? && @lines.empty?
                begin
                  while true
                    @start_reading_time ||= Fluent::Clock.now
                    group_watcher&.update_reading_time(@path)

                    data = io.readpartial(bytes_to_read, iobuf)
                    @eof = false
                    @number_bytes_read += data.bytesize
                    @fifo << data
                    @fifo.sync_base(io.pos)

                    n_lines_before_read = @lines.size
                    has_skipped_line = @fifo.read_lines(@lines) || has_skipped_line
                    group_watcher&.update_lines_read(@path, @lines.size - n_lines_before_read)

                    group_watcher_limit = group_watcher&.limit_lines_reached?(@path)
                    @log.debug "Reading Limit exceeded #{@path} #{group_watcher.number_lines_read}" if group_watcher_limit

                    if group_watcher_limit || limit_bytes_per_second_reached? || should_shutdown_now?
                      # Just get out from tailing loop.
                      @metrics.throttled.inc if group_watcher_limit || limit_bytes_per_second_reached?
                      read_more = false
                      break
                    end

                    if @lines.size >= @read_lines_limit
                      # not to use too much memory in case the file is very large
                      read_more = true
                      break
                    end
                  end
                rescue EOFError
                  @eof = true
                ensure
                  iobuf.clear
                end
              end

              if @lines.empty?
                @watcher.pe.update_pos(io.pos - @fifo.reading_bytesize) if has_skipped_line
              else
                received = @receive_lines.call(@lines, @watcher)
                if received == ASYNC_PENDING
                  @pending_position = io.pos - @fifo.reading_bytesize
                  @pending_read_more ||= read_more
                  @async_pending = true
                  read_more = false
                elsif received
                  @watcher.pe.update_pos(io.pos - @fifo.reading_bytesize)
                  @lines.clear
                  @pending_read_more = false
                else
                  # A rejected worker submission must retain the continuation for retry.
                  @pending_read_more ||= read_more
                  read_more = false
                end
              end
            end while read_more
          end
        end

        def open
          io = Fluent::FileWrapper.open(@path)
          io.seek(@watcher.pe.read_pos + @fifo.reading_bytesize)
          @metrics.opened.inc
          io
        rescue RangeError
          io.close if io
          raise WatcherSetupError, "seek error with #{@path}: file position = #{@watcher.pe.read_pos.to_s(16)}, reading bytesize = #{@fifo.reading_bytesize.to_s(16)}"
        rescue Errno::EACCES => e
          @log.warn "#{e}"
          nil
        rescue Errno::ENOENT
          nil
        end

        def with_io
          if @open_on_every_update
            io = open
            begin
              yield io
            ensure
              io.close unless io.nil?
            end
          else
            @io ||= open
            yield @io
            @eof = true if @io.nil?
          end
        rescue WatcherSetupError => e
          close
          @eof = true
          raise e
        rescue
          @log.error $!.to_s
          @log.error_backtrace
          close
          @eof = true
        end
      end

      class NullIOHandler
        def initialize
        end

        def ready_to_shutdown(shutdown_start_time = nil)
        end

        def io
        end

        def on_notify
        end

        def close
        end

        def opened?
          false
        end

        def eof?
          true
        end

        def drained?
          true
        end

        def pending?
          false
        end
      end
    end
  end
end
