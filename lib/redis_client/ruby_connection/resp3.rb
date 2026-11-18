# frozen_string_literal: true

class RedisClient
  module RESP3
    module_function

    Error = Class.new(RedisClient::Error)
    UnknownType = Class.new(Error)
    SyntaxError = Class.new(Error)

    # Raised when a reply exceeds the decode budget. Unlike SyntaxError,
    # which denotes corrupt input, the reply may be well formed but
    # accepting it would require more memory than the client is willing
    # to spend.
    BudgetExceeded = Class.new(Error)

    # Decode budget for a single reply. All limits are checked *before*
    # allocating, so a malicious or broken peer can't trick the client
    # into pre-allocating huge arrays or strings based on a forged
    # length header.
    #
    # Limits can be disabled individually by setting them to `nil`.
    class Budget
      DEFAULT_MAX_STRING_BYTES = 512 * 1024 * 1024 # 512 MiB, mirrors Redis' proto-max-bulk-len
      DEFAULT_MAX_AGGREGATE_ELEMENTS = 4_194_304 # 2**22
      DEFAULT_MAX_DEPTH = 128
      DEFAULT_MAX_TOTAL_BYTES = 512 * 1024 * 1024 # 512 MiB

      attr_reader :max_string_bytes, :max_aggregate_elements, :max_depth, :max_total_bytes

      def initialize(
        max_string_bytes: DEFAULT_MAX_STRING_BYTES,
        max_aggregate_elements: DEFAULT_MAX_AGGREGATE_ELEMENTS,
        max_depth: DEFAULT_MAX_DEPTH,
        max_total_bytes: DEFAULT_MAX_TOTAL_BYTES
      )
        {
          max_string_bytes: max_string_bytes,
          max_aggregate_elements: max_aggregate_elements,
          max_depth: max_depth,
          max_total_bytes: max_total_bytes,
        }.each do |name, value|
          unless value.nil? || (value.is_a?(Integer) && value >= 0)
            raise ArgumentError, "#{name} must be a non-negative Integer or nil, got: #{value.inspect}"
          end
        end

        @max_string_bytes = max_string_bytes
        @max_aggregate_elements = max_aggregate_elements
        @max_depth = max_depth
        @max_total_bytes = max_total_bytes
      end

      def self.wrap(value)
        case value
        when nil
          DEFAULT
        when Budget
          value
        when Hash
          new(**value)
        else
          raise ArgumentError, "budget must be a RedisClient::RESP3::Budget, a Hash or nil, got: #{value.inspect}"
        end
      end

      # Per-reply mutable decoding state, tracking how much of the budget
      # has been consumed so far.
      class Session
        # Approximate framing cost charged to the total byte budget for
        # every decoded element, so that replies made of myriads of tiny
        # elements are bounded as well.
        ELEMENT_COST = 4

        def initialize(budget)
          @budget = budget
          @total_bytes = 0
          @depth = 0
        end

        def max_string_bytes
          @budget.max_string_bytes
        end

        def consume(bytes)
          @total_bytes += bytes
          if (limit = @budget.max_total_bytes) && @total_bytes > limit
            raise BudgetExceeded, "RESP3 reply exceeds the total byte budget of #{limit} bytes"
          end
        end

        def check_string_size(bytesize)
          if (limit = @budget.max_string_bytes) && bytesize > limit
            raise BudgetExceeded,
              "RESP3 string of #{bytesize} bytes exceeds the string budget of #{limit} bytes"
          end
        end

        def check_aggregate_size(size)
          if (limit = @budget.max_aggregate_elements) && size > limit
            raise BudgetExceeded,
              "RESP3 aggregate of #{size} elements exceeds the aggregate budget of #{limit} elements"
          end
        end

        def enter
          @depth += 1
          if (limit = @budget.max_depth) && @depth > limit
            raise BudgetExceeded, "RESP3 reply nesting exceeds the depth budget of #{limit} levels"
          end
        end

        def leave
          @depth -= 1
        end
      end

      def session
        Session.new(self)
      end

      DEFAULT = new.freeze
    end

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

    def load(io, budget: nil)
      parse(io, Budget.wrap(budget).session)
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

    # Lines holding scalar values (booleans, numbers) should never be
    # anywhere near this large, so they are capped independently of the
    # string budget.
    MAX_SCALAR_LINE_BYTES = 128

    def parse(io, session = nil)
      session ||= Budget::DEFAULT.session
      session.consume(Budget::Session::ELEMENT_COST)

      type = io.getbyte
      if type == 35 # '#'.ord
        parse_boolean(io, session)
      elsif type == 36 # '$'.ord
        parse_blob(io, session)
      elsif type == 43 # '+'.ord
        parse_string(io, session)
      elsif type == 61 # '='.ord
        parse_verbatim_string(io, session)
      elsif type == 45 # '-'.ord
        parse_error(io, session)
      elsif type == 58 # ':'.ord
        parse_integer(io, session)
      elsif type == 40 # '('.ord
        parse_integer(io, session)
      elsif type == 44 # ','.ord
        parse_double(io, session)
      elsif type == 95 # '_'.ord
        parse_null(io, session)
      elsif type == 42 # '*'.ord
        parse_array(io, session)
      elsif type == 37 # '%'.ord
        parse_map(io, session)
      elsif type == 126 # '~'.ord
        parse_set(io, session)
      elsif type == 62 # '>'.ord
        parse_array(io, session)
      elsif type.nil?
        raise SyntaxError, "Unexpected end of stream"
      else
        raise UnknownType, "Unknown sigil type: #{type.chr.inspect}"
      end
    end

    def parse_string(io, session)
      str = io.gets_chomp(session.max_string_bytes)
      session.consume(str.bytesize)
      str.force_encoding(Encoding::BINARY) unless str.valid_encoding?
      str.freeze
    end

    def parse_error(io, session)
      CommandError.parse(parse_string(io, session))
    end

    def parse_boolean(io, _session)
      case value = io.gets_chomp(MAX_SCALAR_LINE_BYTES)
      when "t"
        true
      when "f"
        false
      else
        raise SyntaxError, "Expected `t` or `f` after `#`, got: #{value}"
      end
    end

    def parse_array(io, session)
      size = io.gets_integer
      return if size == -1 # RESP2 null array

      if size < 0
        raise SyntaxError, "Invalid array size: #{size}"
      end

      parse_sequence(io, session, size)
    end

    def parse_set(io, session)
      size = io.gets_integer
      if size < 0 # sets have no null form
        raise SyntaxError, "Invalid set size: #{size}"
      end

      parse_sequence(io, session, size)
    end

    def parse_map(io, session)
      size = io.gets_integer
      if size < 0 # maps have no null form
        raise SyntaxError, "Invalid map size: #{size}"
      end

      session.check_aggregate_size(size)
      session.enter
      begin
        hash = {}
        size.times do
          hash[parse(io, session).freeze] = parse(io, session)
        end
        hash
      ensure
        session.leave
      end
    end

    def parse_sequence(io, session, size)
      session.check_aggregate_size(size)
      session.enter
      begin
        array = Array.new(size)
        size.times do |index|
          array[index] = parse(io, session)
        end
        array
      ensure
        session.leave
      end
    end

    def parse_integer(io, session)
      str = io.gets_chomp(MAX_SCALAR_LINE_BYTES)
      session.consume(str.bytesize)
      Integer(str)
    rescue ArgumentError
      raise SyntaxError, "Invalid integer: #{str.inspect}"
    end

    def parse_double(io, session)
      case value = io.gets_chomp(MAX_SCALAR_LINE_BYTES)
      when "nan"
        Float::NAN
      when "inf"
        Float::INFINITY
      when "-inf"
        -Float::INFINITY
      else
        session.consume(value.bytesize)
        begin
          Float(value)
        rescue ArgumentError
          raise SyntaxError, "Invalid double: #{value.inspect}"
        end
      end
    end

    def parse_null(io, _session)
      unless io.getbyte == 13 && io.getbyte == 10 # "\r\n"
        raise SyntaxError, "Expected CRLF after `_`"
      end
      nil
    end

    def parse_blob(io, session)
      bytesize = io.gets_integer
      return if bytesize == -1 # RESP2 null string

      if bytesize < 0
        raise SyntaxError, "Invalid blob string size: #{bytesize}"
      end

      session.check_string_size(bytesize)
      session.consume(bytesize)
      str = io.read_chomp(bytesize)
      str.force_encoding(Encoding::BINARY) unless str.valid_encoding?
      str
    end

    def parse_verbatim_string(io, session)
      blob = parse_blob(io, session)
      if blob.nil? || blob.bytesize < 4
        raise SyntaxError, "Invalid verbatim string"
      end
      blob.byteslice(4..-1)
    end
  end
end
