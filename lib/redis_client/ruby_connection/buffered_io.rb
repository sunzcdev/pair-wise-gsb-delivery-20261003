# frozen_string_literal: true

require "io/wait" unless IO.method_defined?(:wait_readable) && IO.method_defined?(:wait_writable)

class RedisClient
  class RubyConnection
    class BufferedIO
      EOL = "\r\n".b.freeze
      EOL_SIZE = EOL.bytesize

      attr_accessor :read_timeout, :write_timeout

      if String.method_defined?(:byteindex) # Ruby 3.2+
        ENCODING = Encoding::UTF_8

        def initialize(io, read_timeout:, write_timeout:, chunk_size: 4096)
          @io = io
          @buffer = +""
          @offset = 0
          @chunk_size = chunk_size
          @read_timeout = read_timeout
          @write_timeout = write_timeout
          @blocking_reads = false
        end

        def gets_chomp(limit = nil)
          fill_buffer(false) if @offset >= @buffer.bytesize
          until eol_index = @buffer.byteindex(EOL, @offset)
            check_line_limit(limit)
            fill_buffer(false)
          end

          line = @buffer.byteslice(@offset, eol_index - @offset)
          if limit && line.bytesize > limit
            raise RESP3::BudgetExceeded, "RESP3 line of #{line.bytesize} bytes exceeds the limit of #{limit} bytes"
          end
          @offset = eol_index + EOL_SIZE
          line
        end

        def read_chomp(bytes)
          ensure_remaining(bytes + EOL_SIZE)
          str = @buffer.byteslice(@offset, bytes)
          verify_eol(@offset + bytes)
          @offset += bytes + EOL_SIZE
          str
        end

        private def ensure_line
          fill_buffer(false) if @offset >= @buffer.bytesize
          until @buffer.byteindex(EOL, @offset)
            fill_buffer(false)
          end
        end
      else
        ENCODING = Encoding::BINARY

        def initialize(io, read_timeout:, write_timeout:, chunk_size: 4096)
          @io = io
          @buffer = "".b
          @offset = 0
          @chunk_size = chunk_size
          @read_timeout = read_timeout
          @write_timeout = write_timeout
          @blocking_reads = false
        end

        def gets_chomp(limit = nil)
          fill_buffer(false) if @offset >= @buffer.bytesize
          until eol_index = @buffer.index(EOL, @offset)
            check_line_limit(limit)
            fill_buffer(false)
          end

          line = @buffer.byteslice(@offset, eol_index - @offset)
          if limit && line.bytesize > limit
            raise RESP3::BudgetExceeded, "RESP3 line of #{line.bytesize} bytes exceeds the limit of #{limit} bytes"
          end
          @offset = eol_index + EOL_SIZE
          line
        end

        def read_chomp(bytes)
          ensure_remaining(bytes + EOL_SIZE)
          str = @buffer.byteslice(@offset, bytes)
          verify_eol(@offset + bytes)
          @offset += bytes + EOL_SIZE
          str.force_encoding(Encoding::UTF_8)
        end

        private def ensure_line
          fill_buffer(false) if @offset >= @buffer.bytesize
          until @buffer.index(EOL, @offset)
            fill_buffer(false)
          end
        end
      end

      def close
        @io.to_io.close
      end

      def closed?
        @io.to_io.closed?
      end

      def eof?
        @offset >= @buffer.bytesize && @io.eof?
      end

      def with_timeout(new_timeout)
        new_timeout = false if new_timeout == 0

        previous_read_timeout = @read_timeout
        previous_blocking_reads = @blocking_reads

        if new_timeout
          @read_timeout = new_timeout
        else
          @blocking_reads = true
        end

        begin
          yield
        ensure
          @read_timeout = previous_read_timeout
          @blocking_reads = previous_blocking_reads
        end
      end

      def skip(offset)
        ensure_remaining(offset)
        @offset += offset
        nil
      end

      def write(string)
        total = remaining = string.bytesize
        loop do
          case bytes_written = @io.write_nonblock(string, exception: false)
          when Integer
            remaining -= bytes_written
            if remaining > 0
              string = string.byteslice(bytes_written..-1)
            else
              return total
            end
          when :wait_readable
            @io.to_io.wait_readable(@read_timeout) or raise(ReadTimeoutError, "Waited #{@read_timeout} seconds")
          when :wait_writable
            @io.to_io.wait_writable(@write_timeout) or raise(WriteTimeoutError, "Waited #{@write_timeout} seconds")
          when nil
            raise Errno::ECONNRESET
          else
            raise "Unexpected `write_nonblock` return: #{bytes.inspect}"
          end
        end
      end

      def getbyte
        unless byte = @buffer.getbyte(@offset)
          ensure_remaining(1)
          byte = @buffer.getbyte(@offset)
        end
        @offset += 1
        byte
      end

      # Strictly parses a (possibly negative) integer terminated by CRLF.
      # Length headers larger than 64 bits are rejected up front, as they
      # could never be allocated anyway.
      #
      # RESP3 error classes are referenced here even though this file
      # doesn't require `resp3.rb`; both files are always loaded together
      # through `redis_client/ruby_connection`.
      def gets_integer
        int = 0
        digits = 0
        negative = false
        offset = @offset
        while true
          chr = @buffer.getbyte(offset)

          if chr.nil?
            ensure_line
            return gets_integer
          elsif chr == 13 # "\r".ord
            if digits == 0
              raise RESP3::SyntaxError, "Expected an integer, got an empty value"
            end

            nxt = @buffer.getbyte(offset + 1)
            if nxt.nil?
              ensure_line
              return gets_integer
            elsif nxt != 10 # "\n".ord
              raise RESP3::SyntaxError, "Expected CRLF after integer"
            end

            @offset = offset + 2
            break
          elsif chr == 45 && offset == @offset # "-".ord, only allowed as the first character
            negative = true
          elsif chr >= 48 && chr <= 57 # between "0".ord and "9".ord
            if digits >= 20
              raise RESP3::BudgetExceeded, "RESP3 integer exceeds the 64-bit range"
            end
            int = (int * 10) + chr - 48
            digits += 1
          else
            raise RESP3::SyntaxError, "Unexpected byte in integer: #{chr.chr.inspect}"
          end
          offset += 1
        end

        negative ? -int : int
      end

      private

      def check_line_limit(limit)
        if limit && @buffer.bytesize - @offset > limit
          raise RESP3::BudgetExceeded, "RESP3 line exceeds the limit of #{limit} bytes"
        end
      end

      def verify_eol(offset)
        unless @buffer.getbyte(offset) == 13 && @buffer.getbyte(offset + 1) == 10 # "\r\n"
          raise RESP3::SyntaxError, "Expected CRLF after blob string"
        end
      end

      def ensure_remaining(bytes)
        needed = bytes - (@buffer.bytesize - @offset)
        if needed > 0
          fill_buffer(true, needed)
        end
      end

      def fill_buffer(strict, size = @chunk_size)
        remaining = size
        buffer_size = @buffer.bytesize
        start = @offset - buffer_size
        empty_buffer = start >= 0
        read_buffer = nil

        loop do
          bytes = if empty_buffer
            @io.read_nonblock([remaining, @chunk_size].max, @buffer, exception: false)
          else
            # We're most likely reading 4kiB or more, hence the read buffer will never be
            # embedded on any recent Ruby implementation, hence we can provide an empty
            # buffer, Ruby will take care of preallocating the required memory.
            read_buffer ||= "".b
            @io.read_nonblock([remaining, @chunk_size].max, read_buffer, exception: false)
          end

          case bytes
          when :wait_readable
            # Ref: https://github.com/redis-rb/redis-client/issues/190
            # SSLSocket always clear the provided buffer, even when it didn't
            # read anything. So we need to reset the offset accordingly.
            if empty_buffer && @buffer.empty?
              @offset -= buffer_size
            end

            unless @io.to_io.wait_readable(@read_timeout)
              raise ReadTimeoutError, "Waited #{@read_timeout} seconds" unless @blocking_reads
            end
          when :wait_writable
            # Ref: https://github.com/redis-rb/redis-client/issues/190
            # SSLSocket always clear the provided buffer, even when it didn't
            # read anything. So we need to reset the offset accordingly.
            if empty_buffer && @buffer.empty?
              @offset -= buffer_size
            end

            @io.to_io.wait_writable(@write_timeout) or raise(WriteTimeoutError, "Waited #{@write_timeout} seconds")
          when nil
            raise EOFError
          else
            if empty_buffer
              @offset = start
              empty_buffer = false
              @buffer.force_encoding(ENCODING) unless @buffer.encoding == ENCODING
            else
              @buffer << bytes.force_encoding(ENCODING)
            end
            remaining -= bytes.bytesize
            if !strict || remaining <= 0
              read_buffer&.clear
              return
            end
          end
        end
      end
    end
  end
end
