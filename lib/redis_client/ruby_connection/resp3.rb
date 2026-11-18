# frozen_string_literal: true

class RedisClient
  module RESP3
    module_function

    Error = Class.new(RedisClient::Error)
    UnknownType = Class.new(Error)
    SyntaxError = Class.new(Error)

    # Holds a decoded reply together with the RESP3 attributes (`|` frame) that
    # were sent right before it.
    #
    # Attribute frames are metadata for the reply that follows, so they are
    # never returned as standalone replies. This wrapper makes the metadata
    # available without being confused with a regular RESP3 map (which decodes
    # to a Ruby Hash).
    class Attribute
      attr_reader :value, :attributes

      def initialize(value, attributes)
        @value = value
        @attributes = attributes.freeze
      end

      def deconstruct
        [value, attributes]
      end

      def deconstruct_keys(_keys)
        { value: value, attributes: attributes }
      end

      def ==(other)
        other.is_a?(Attribute) && value == other.value && attributes == other.attributes
      end
      alias_method :eql?, :==

      def hash
        [Attribute, value, attributes].hash
      end

      # Returns the business value, with any nested attribute wrappers removed.
      def unwrap
        RESP3.unwrap(value)
      end

      def inspect
        "#<#{self.class.name} value=#{value.inspect} attributes=#{attributes.inspect}>"
      end
    end

    EOL = "\r\n".b.freeze
    EOL_SIZE = EOL.bytesize
    ATTRIBUTE_TYPE = 124 # '|'.ord
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

    def load(io, with_attributes: true)
      parse(io, with_attributes)
    end

    # Returns `value` with every attribute wrapper removed, recursively.
    #
    # Plain replies are returned untouched; only `RESP3::Attribute` wrappers,
    # including the ones nested inside aggregates, are stripped.
    def unwrap(value)
      case value
      when Attribute
        unwrap(value.value)
      when Array
        value.map { |element| unwrap(element) }
      when Hash
        value.each_with_object({}) do |(key, element), hash|
          hash[unwrap(key)] = unwrap(element)
        end
      else
        value
      end
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

    def parse(io, with_attributes = true)
      type = io.getbyte

      # Attribute frames are metadata for the reply that follows, so they are
      # never returned as standalone replies: consuming one still leaves the
      # caller with exactly one reply, which keeps pipelined replies aligned.
      # A truncated frame raises like any incomplete reply, it is never dropped
      # to pretend the reply was complete.
      attributes = nil
      if type == ATTRIBUTE_TYPE
        # Frame contents are decoded as plain protocol values: keys keep their
        # protocol type (integer, boolean, blob, ...) and unknown attributes are
        # preserved as-is. A frame nested inside another frame is not meaningful
        # metadata, so it is consumed and dropped like when it is skipped.
        attributes = parse_map(io, false)
        while (type = io.getbyte) == ATTRIBUTE_TYPE
          # Consecutive frames are merged in arrival order, so the frame
          # closest to the reply wins on duplicate keys.
          attributes.merge!(parse_map(io, false))
        end
      end

      value = if type == 35 # '#'.ord
        parse_boolean(io)
      elsif type == 36 # '$'.ord
        parse_blob(io)
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
        parse_array(io, with_attributes)
      elsif type == 37 # '%'.ord
        parse_map(io, with_attributes)
      elsif type == 126 # '~'.ord
        parse_set(io, with_attributes)
      elsif type == 62 # '>'.ord
        parse_array(io, with_attributes)
      elsif type.nil?
        raise SyntaxError, "Unexpected end of stream"
      else
        raise UnknownType, "Unknown sigil type: #{type.chr.inspect}"
      end

      if attributes.nil?
        value
      elsif value.is_a?(CommandError)
        # Errors are raised rather than returned, so their attributes ride on
        # the error object instead of hiding it behind a wrapper. This is done
        # even when the caller skips attribute wrappers: it doesn't add a reply
        # slot, it only enriches the raised error.
        value.attributes = attributes.freeze
        value
      elsif with_attributes
        Attribute.new(value, attributes)
      else
        value
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

    def parse_array(io, with_attributes = true)
      parse_sequence(io, io.gets_integer, with_attributes)
    end

    def parse_set(io, with_attributes = true)
      parse_sequence(io, io.gets_integer, with_attributes)
    end

    def parse_map(io, with_attributes = true)
      hash = {}
      io.gets_integer.times do
        hash[parse(io, with_attributes).freeze] = parse(io, with_attributes)
      end
      hash
    end

    def parse_sequence(io, size, with_attributes = true)
      return if size < 0 # RESP2 nil

      array = Array.new(size)
      size.times do |index|
        array[index] = parse(io, with_attributes)
      end
      array
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

    def parse_blob(io)
      bytesize = io.gets_integer
      return if bytesize < 0 # RESP2 nil type

      str = io.read_chomp(bytesize)
      str.force_encoding(Encoding::BINARY) unless str.valid_encoding?
      str
    end

    def parse_verbatim_string(io)
      blob = parse_blob(io)
      blob.byteslice(4..-1)
    end
  end
end
