# frozen_string_literal: true

class RedisClient
  module RESP3
    module_function

    Error = Class.new(RedisClient::Error)
    UnknownType = Class.new(Error)
    SyntaxError = Class.new(Error)

    # Returned by the IO layer when the streamed (unknown length) marker `?`
    # is read instead of an integer, e.g. `$?\r\n` or `*?\r\n`.
    STREAMED = :streamed

    EOL = "\r\n".b.freeze
    EOL_SIZE = EOL.bytesize
    DUMP_TYPES = {
      String => :dump_string,
      Symbol => :dump_symbol,
      Integer => :dump_numeric,
      Float => :dump_numeric,
    }.freeze
    INTEGER_RANGE = ((((2**64) / 2) * -1)..(((2**64) / 2) - 1)).freeze

    def dump(command, buffer = nil)
      buffer ||= new_buffer
      command = command.flat_map do |element|
        case element
        when Hash
          element.flatten
        else
          element
        end
      end
      dump_array(command, buffer)
    end

    def load(io, &block)
      parse(io, &block)
    end

    def new_buffer
      String.new(encoding: Encoding::BINARY, capacity: 127)
    end

    def dump_any(object, buffer)
      method = DUMP_TYPES.fetch(object.class) do |unexpected_class|
        if superclass = DUMP_TYPES.keys.find { |t| t > unexpected_class }
          DUMP_TYPES[superclass]
        else
          raise TypeError, "Unsupported command argument type: #{unexpected_class}"
        end
      end
      send(method, object, buffer)
    end

    def dump_array(array, buffer)
      buffer << '*' << array.size.to_s << EOL
      array.each do |item|
        dump_any(item, buffer)
      end
      buffer
    end

    def dump_numeric(numeric, buffer)
      dump_string(numeric.to_s, buffer)
    end

    def dump_string(string, buffer)
      string = string.b unless string.ascii_only?
      buffer << '$' << string.bytesize.to_s << EOL << string << EOL
    end

    if Symbol.method_defined?(:name)
      def dump_symbol(symbol, buffer)
        dump_string(symbol.name, buffer)
      end
    else
      def dump_symbol(symbol, buffer)
        dump_string(symbol.to_s, buffer)
      end
    end

    def parse(io, &block)
      type = io.getbyte
      if type.nil?
        raise SyntaxError, "Unexpected end of stream"
      end

      parse_type(io, type, &block)
    end

    def parse_type(io, type, &block)
      if type == 35 # '#'.ord
        parse_boolean(io)
      elsif type == 36 # '$'.ord
        parse_blob(io, &block)
      elsif type == 43 # '+'.ord
        parse_string(io)
      elsif type == 61 # '='.ord
        parse_verbatim_string(io)
      elsif type == 45 # '-'.ord
        parse_error(io)
      elsif type == 58 # ':'.ord
        parse_integer(io)
      elsif type == 40 # '('.ord
        parse_integer(io)
      elsif type == 44 # ','.ord
        parse_double(io)
      elsif type == 95 # '_'.ord
        parse_null(io)
      elsif type == 42 # '*'.ord
        parse_array(io, &block)
      elsif type == 37 # '%'.ord
        parse_map(io, &block)
      elsif type == 126 # '~'.ord
        parse_set(io, &block)
      elsif type == 62 # '>'.ord
        parse_array(io, &block)
      else
        raise UnknownType, "Unknown sigil type: #{type.chr.inspect}"
      end
    end

    def parse_string(io)
      str = io.gets_chomp
      str.force_encoding(Encoding::BINARY) unless str.valid_encoding?
      str.freeze
    end

    def parse_error(io)
      CommandError.parse(parse_string(io))
    end

    def parse_boolean(io)
      case value = io.gets_chomp
      when "t"
        true
      when "f"
        false
      else
        raise SyntaxError, "Expected `t` or `f` after `#`, got: #{value}"
      end
    end

    def parse_array(io, &block)
      parse_sequence(io, io.gets_integer, &block)
    end

    def parse_set(io, &block)
      parse_sequence(io, io.gets_integer, &block)
    end

    def parse_map(io, &block)
      size = io.gets_integer
      if STREAMED == size
        return parse_streamed_map(io, &block)
      end

      hash = {}
      size.times do
        hash[parse(io, &block).freeze] = parse(io, &block)
      end
      hash
    end

    def parse_sequence(io, size, &block)
      if STREAMED == size
        return parse_streamed_sequence(io, &block)
      end
      return if size < 0 # RESP2 nil

      array = Array.new(size)
      size.times do |index|
        array[index] = parse(io, &block)
      end
      array
    end

    # Parses a streamed aggregate (`*?`, `~?`, `>?`), i.e. a sequence of
    # replies terminated by a `.\r\n` marker rather than a declared length.
    def parse_streamed_sequence(io, &block)
      array = []
      while type = parse_streamed_element_type(io)
        array << parse_type(io, type, &block)
      end
      array
    end

    # Parses a streamed map (`%?`): alternating keys and values until the
    # `.\r\n` terminator.
    def parse_streamed_map(io, &block)
      hash = {}
      while type = parse_streamed_element_type(io)
        hash[parse_type(io, type, &block).freeze] = parse(io, &block)
      end
      hash
    end

    # Reads the sigil of the next element inside a streamed aggregate.
    # Returns nil once the `.` terminator has been fully consumed, so not
    # a single byte of the next reply is swallowed.
    def parse_streamed_element_type(io)
      type = io.getbyte
      if type.nil?
        raise SyntaxError, "Unexpected end of stream inside streamed aggregate"
      elsif type == 46 # '.'.ord
        expect_eol(io)
        nil
      else
        type
      end
    end

    def expect_eol(io)
      unless io.getbyte == 13 && io.getbyte == 10 # "\r\n"
        raise SyntaxError, "Expected CRLF after streamed aggregate terminator"
      end
      nil
    end

    def parse_integer(io)
      Integer(io.gets_chomp)
    end

    def parse_double(io)
      case value = io.gets_chomp
      when "nan"
        Float::NAN
      when "inf"
        Float::INFINITY
      when "-inf"
        -Float::INFINITY
      else
        Float(value)
      end
    end

    def parse_null(io)
      io.skip(EOL_SIZE)
      nil
    end

    def parse_blob(io, &block)
      bytesize = io.gets_integer
      if STREAMED == bytesize
        return parse_streamed_string(io, &block)
      end
      return if bytesize < 0 # RESP2 nil type

      str = io.read_chomp(bytesize)
      str.force_encoding(Encoding::BINARY) unless str.valid_encoding?
      str
    end

    # Parses a streamed string (`$?`): a succession of `;<bytesize>\r\n<chunk>\r\n`
    # chunks terminated by a zero-length chunk (`;0\r\n`). The total size is never
    # known in advance, parsing solely relies on the protocol terminator.
    #
    # When a block is given, each chunk is yielded as a binary string as soon as
    # it is received and the whole string is never buffered, in which case this
    # method returns nil. Otherwise chunks are concatenated and returned like a
    # regular blob string.
    def parse_streamed_string(io)
      buffer = new_buffer unless block_given?
      loop do
        type = io.getbyte
        unless type == 59 # ';'.ord
          raise SyntaxError, "Expected streamed string chunk, got: #{type.nil? ? "end of stream" : type.chr.inspect}"
        end

        chunk_size = io.gets_integer
        break if chunk_size == 0 # `;0\r\n` terminates the streamed string

        unless chunk_size.is_a?(Integer) && chunk_size > 0
          raise SyntaxError, "Invalid streamed string chunk size: #{chunk_size.inspect}"
        end

        # Chunk boundaries may split multi-byte characters, hence chunks are
        # always binary. The encoding of the reassembled string is fixed up below.
        chunk = io.read_chomp(chunk_size)
        chunk.force_encoding(Encoding::BINARY)
        if block_given?
          yield chunk
        else
          buffer << chunk
        end
      end

      if buffer
        buffer.force_encoding(Encoding::UTF_8)
        buffer.force_encoding(Encoding::BINARY) unless buffer.valid_encoding?
        buffer
      end
    end

    def parse_verbatim_string(io)
      blob = parse_blob(io)
      blob.byteslice(4..-1)
    end
  end
end
