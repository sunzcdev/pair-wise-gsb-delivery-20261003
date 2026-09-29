# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

module Rack
  module Session
    class Cookie < Abstract::PersistedSecure
      # A safe session coder with an explicit, versioned, self-describing
      # format.
      #
      # Only the following types are supported:
      #
      # * String
      # * true / false
      # * nil
      # * Integer (including arbitrarily large integers)
      # * Finite Float
      # * Time (preserving the UTC offset)
      # * Array
      # * Hash with String keys
      #
      # Decoding never performs constant lookup, never uses Marshal, and
      # never invokes application callbacks such as initializers or
      # finalizers. It only ever constructs instances of the types listed
      # above. Duplicate hash keys, unknown type tags, unsupported format
      # versions, malformed input and excessively deep structures all raise
      # DecodeError deterministically.
      #
      # Encoding builds the complete payload in memory before returning it,
      # so a failure can never produce a partially written session cookie.
      # Unsupported types, non-finite floats, non-String hash keys, circular
      # structures and excessively deep structures raise EncodeError.
      #
      # Example:
      #
      #   Rack::Session::Cookie.new(application, {
      #     coder: Rack::Session::Cookie::Safe.new,
      #     let_coder_handle_secure_encoding: true,
      #   })
      #
      class Safe
        # The format version. Cookies prefixed with any other version are
        # rejected deterministically.
        VERSION = 1

        MAGIC = "RSC"
        HEADER = "#{MAGIC}#{VERSION}".freeze

        # The maximum nesting depth of arrays and hashes.
        MAXIMUM_DEPTH = 100

        class Error < StandardError; end

        # Raised when a session cannot be encoded.
        class EncodeError < Error; end

        # Raised when cookie data cannot be decoded.
        class DecodeError < Error; end

        # Type tags, as byte values:
        TAG_NIL = ?n.ord
        TAG_TRUE = ?t.ord
        TAG_FALSE = ?f.ord
        TAG_INTEGER = ?i.ord
        TAG_FLOAT = ?d.ord
        TAG_STRING = ?s.ord
        TAG_TIME = ?T.ord
        TAG_ARRAY = ?a.ord
        TAG_HASH = ?h.ord

        SEMICOLON = ?;.ord
        COLON = ?:.ord

        INTEGER_PATTERN = /\A-?\d+\z/.freeze
        COUNT_PATTERN = /\A\d+\z/.freeze

        NANOSECONDS_PER_SECOND = 1_000_000_000

        def encode(object)
          # The payload is built in a local buffer and only returned once
          # encoding has fully succeeded, so a failure can never leak a
          # partially encoded session.
          buffer = String.new(encoding: Encoding::BINARY)
          buffer << HEADER
          encode_value(object, buffer, 0, {})
          buffer
        end

        def decode(string)
          return unless string

          reader = Reader.new(string)

          unless reader.read_header?(HEADER)
            raise DecodeError, "unsupported session cookie format or version"
          end

          value = decode_value(reader, 0)

          unless reader.eof?
            raise DecodeError, "trailing data after session cookie payload"
          end

          value
        end

        private

        def encode_value(value, buffer, depth, seen)
          case value
          when nil
            buffer << TAG_NIL
          when true
            buffer << TAG_TRUE
          when false
            buffer << TAG_FALSE
          when Integer
            buffer << TAG_INTEGER << value.to_s << SEMICOLON
          when Float
            unless value.finite?
              raise EncodeError, "cannot encode non-finite float: #{value}"
            end
            buffer << TAG_FLOAT << value.to_s << SEMICOLON
          when String
            buffer << TAG_STRING << value.bytesize.to_s << COLON
            buffer << value
          when Time
            buffer << TAG_TIME <<
              value.to_i.to_s << SEMICOLON <<
              value.nsec.to_s << SEMICOLON <<
              value.utc_offset.to_s << SEMICOLON
          when Array
            encode_composite(value, depth, seen) do
              buffer << TAG_ARRAY << value.size.to_s << COLON
              value.each do |element|
                encode_value(element, buffer, depth + 1, seen)
              end
            end
          when Hash
            encode_composite(value, depth, seen) do
              buffer << TAG_HASH << value.size.to_s << COLON
              value.each do |key, element|
                unless key.is_a?(String)
                  raise EncodeError, "hash keys must be strings, got: #{key.class}"
                end
                encode_value(key, buffer, depth + 1, seen)
                encode_value(element, buffer, depth + 1, seen)
              end
            end
          else
            raise EncodeError, "unsupported type: #{value.class}"
          end
        end

        def encode_composite(value, depth, seen)
          if depth >= MAXIMUM_DEPTH
            raise EncodeError, "structure exceeds maximum depth of #{MAXIMUM_DEPTH}"
          end

          object_id = value.object_id

          if seen.key?(object_id)
            raise EncodeError, "cannot encode circular structure"
          end

          seen[object_id] = true

          begin
            yield
          ensure
            seen.delete(object_id)
          end
        end

        def decode_value(reader, depth)
          case reader.read_byte
          when TAG_NIL
            nil
          when TAG_TRUE
            true
          when TAG_FALSE
            false
          when TAG_INTEGER
            decode_integer(reader)
          when TAG_FLOAT
            decode_float(reader)
          when TAG_STRING
            decode_string(reader)
          when TAG_TIME
            decode_time(reader)
          when TAG_ARRAY
            decode_array(reader, depth)
          when TAG_HASH
            decode_hash(reader, depth)
          else
            raise DecodeError, "unknown type tag: #{reader.previous_byte.chr.inspect}"
          end
        end

        def decode_integer(reader)
          token = reader.read_token(SEMICOLON)

          unless INTEGER_PATTERN.match?(token)
            raise DecodeError, "invalid integer: #{token.inspect}"
          end

          Integer(token, 10)
        end

        def decode_float(reader)
          token = reader.read_token(SEMICOLON)

          value = begin
            Float(token)
          rescue ArgumentError
            raise DecodeError, "invalid float: #{token.inspect}"
          end

          unless value.finite?
            raise DecodeError, "non-finite float: #{token.inspect}"
          end

          value
        end

        def decode_string(reader)
          size = decode_count(reader)
          reader.read_bytes(size).force_encoding(Encoding::UTF_8)
        end

        def decode_time(reader)
          epoch = decode_integer(reader)
          nanoseconds = decode_integer(reader)
          utc_offset = decode_integer(reader)

          unless nanoseconds >= 0 && nanoseconds < NANOSECONDS_PER_SECOND
            raise DecodeError, "nanoseconds out of range: #{nanoseconds}"
          end

          begin
            Time.at(Rational(epoch, 1) + Rational(nanoseconds, NANOSECONDS_PER_SECOND)).getlocal(utc_offset)
          rescue ArgumentError, RangeError
            raise DecodeError, "invalid time: epoch=#{epoch} nanoseconds=#{nanoseconds} utc_offset=#{utc_offset}"
          end
        end

        def decode_array(reader, depth)
          check_depth!(depth)

          count = decode_count(reader)

          # Every element requires at least one byte, so a count larger than
          # the remaining input can never be satisfied:
          if count > reader.remaining
            raise DecodeError, "array size #{count} exceeds remaining input"
          end

          array = []
          count.times do
            array << decode_value(reader, depth + 1)
          end
          array
        end

        def decode_hash(reader, depth)
          check_depth!(depth)

          count = decode_count(reader)

          # Every key/value pair requires at least two bytes:
          if count * 2 > reader.remaining
            raise DecodeError, "hash size #{count} exceeds remaining input"
          end

          hash = {}
          count.times do
            key = decode_value(reader, depth + 1)

            unless key.is_a?(String)
              raise DecodeError, "hash key is not a string: #{key.inspect}"
            end

            if hash.key?(key)
              raise DecodeError, "duplicate hash key: #{key.inspect}"
            end

            hash[key] = decode_value(reader, depth + 1)
          end
          hash
        end

        def decode_count(reader)
          token = reader.read_token(COLON)

          unless COUNT_PATTERN.match?(token)
            raise DecodeError, "invalid element count: #{token.inspect}"
          end

          Integer(token, 10)
        end

        def check_depth!(depth)
          if depth >= MAXIMUM_DEPTH
            raise DecodeError, "structure exceeds maximum depth of #{MAXIMUM_DEPTH}"
          end
        end

        # A byte-oriented reader over the cookie payload. It performs no
        # constant lookup and constructs no objects other than strings and
        # integers from the raw bytes.
        class Reader
          attr_reader :previous_byte

          def initialize(string)
            @string = string
            @length = string.bytesize
            @index = 0
            @previous_byte = nil
          end

          def eof?
            @index >= @length
          end

          def remaining
            @length - @index
          end

          def read_header?(header)
            if @string.byteslice(0, header.bytesize) == header
              @index = header.bytesize
              true
            else
              false
            end
          end

          def read_byte
            if eof?
              raise DecodeError, "unexpected end of input"
            end

            @previous_byte = @string.getbyte(@index)
            @index += 1
            @previous_byte
          end

          # Read bytes up to (but not including) the given delimiter byte,
          # returning them as a binary string.
          def read_token(delimiter)
            start = @index

            while @index < @length
              if @string.getbyte(@index) == delimiter
                token = @string.byteslice(start, @index - start)
                token = token.b if token
                @index += 1
                return token
              end

              @index += 1
            end

            raise DecodeError, "unexpected end of input"
          end

          def read_bytes(count)
            if count > remaining
              raise DecodeError, "unexpected end of input"
            end

            bytes = @string.byteslice(@index, count)
            @index += count
            bytes
          end
        end
      end
    end
  end
end
