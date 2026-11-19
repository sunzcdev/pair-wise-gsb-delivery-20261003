# encoding: binary
# frozen_string_literal: true

require "amq/protocol/type_constants"

module AMQ
  module Protocol
    class Table
      # Raised when something is wrong with a field table codec itself rather
      # than with the data being processed.
      class TableCodecError < ArgumentError; end

      # Raised when a custom codec is registered for a type code that already
      # has a codec in the same collection.
      class CodecConflictError < TableCodecError; end

      # Raised when a type code is encountered on decode that no codec in the
      # frozen collection knows how to read.
      class UnknownTypeError < TableCodecError; end

      # Raised when a custom decoder reports an offset that cannot be true.
      class InvalidOffsetError < TableCodecError; end

      # A single field-table type code together with the decoding, encoding and
      # length rules that apply to it for the duration of one field table read
      # or write.
      class Codec
        attr_reader :type, :decoder, :encoder, :sizer, :matcher

        # @param type [String] single byte type code
        # @param decoder [#call] receives (data, offset), returns [value, new_offset]
        # @param encoder [#call] receives a value, returns the encoded bytes
        #   *without* the leading type byte
        # @param sizer [#call, nil] receives a value, returns its encoded size
        #   in bytes without the leading type byte
        # @param match [#call, nil] receives a value and returns true when this
        #   codec is responsible for encoding that Ruby value; without a matcher
        #   the codec is reachable only through its type code on decode
        def initialize(type:, decoder: nil, encoder: nil, sizer: nil, match: nil)
          if !type.is_a?(::String) || type.bytesize != 1
            raise TableCodecError, "type code must be a single byte, got #{type.inspect}"
          end

          @type    = type.dup.freeze
          @decoder = decoder
          @encoder = encoder
          @sizer   = sizer
          @matcher = match
          freeze
        end

        def decodable?
          !@decoder.nil?
        end

        def encodable?
          !@encoder.nil?
        end
      end # Codec

      # An immutable, explicitly scoped collection of field table codecs. A set
      # is frozen when built and never mutates afterwards, so registering a
      # plugin codec for one field table cannot change the behavior of any
      # other session. Nested tables and arrays are decoded through the very
      # same set instance.
      class CodecSet
        include TypeConstants

        # Builds the codec set used by a field table operation.
        #
        # @param custom_codecs [Array<Codec>] plugin codecs layered on top of
        #   the built-in codecs
        # @raise [CodecConflictError] if a custom codec reuses a type code that
        #   is already taken in this set, including by another custom codec
        def self.build(custom_codecs = nil)
          codecs = built_in_codecs.dup

          Array(custom_codecs).each do |codec|
            unless codec.is_a?(Codec)
              raise TableCodecError, "custom codec must be an AMQ::Protocol::Table::Codec, got #{codec.class}"
            end

            if codecs.key?(codec.type)
              raise CodecConflictError,
                    "type code #{codec.type.inspect} is already registered in this codec set"
            end

            codecs[codec.type] = codec
          end

          new(codecs)
        end

        def self.built_in_codecs
          AMQ::Protocol::TableValueDecoder.built_in_codecs
        end

        def initialize(codecs)
          @codecs = codecs.freeze
          freeze
        end

        # Reads one field value (without its leading type byte) at offset.
        #
        # Nested tables and arrays are decoded in place, so offsets are always
        # absolute within the shared buffer and only the buffer size is a valid
        # hard boundary; the container loops enforce their own narrower limits.
        #
        # @return [Array(Object, Integer)] the decoded value and the absolute
        #   offset just past the consumed bytes
        # @raise [UnknownTypeError] if the type code has no decoder, in which
        #   case no bytes are skipped because the value length is unknown
        # @raise [InvalidOffsetError] if a custom decoder reports an offset
        #   that goes backwards or past the end of the data
        def decode_value(data, offset, type, _limit)
          codec = @codecs[type]

          if codec.nil? || !codec.decodable?
            # The length of an unknown value cannot be determined, so skipping
            # bytes would mean guessing and corrupting every later field.
            raise UnknownTypeError,
                  "unknown field table type code #{type.inspect} at offset #{offset}; cannot determine value length"
          end

          value, new_offset = invoke_decoder(codec, data, offset, type)

          validate_offset!(new_offset, offset, data.bytesize, type)

          [value, new_offset]
        end

        # Encodes one field value, choosing the first custom codec that claims
        # it before falling back to the built-in encoding rules.
        #
        # @return [Array(String, String)] the type code and the encoded bytes
        def encode_value(value)
          custom = custom_codec_for(value)

          if custom
            encoded = custom.encoder.call(value)
            verify_sizer!(custom, value, encoded)
            return custom.type, encoded
          end

          encoded = AMQ::Protocol::TableValueEncoder.encode(value, self)
          [encoded.byteslice(0, 1), encoded.byteslice(1..-1)]
        end

        # Size of an encoded field value in bytes, including the type byte.
        def value_size(value)
          custom = custom_codec_for(value)

          unless custom.nil?
            return 1 + custom.sizer.call(value) unless custom.sizer.nil?

            return 1 + custom.encoder.call(value).bytesize
          end

          AMQ::Protocol::TableValueEncoder.field_value_size(value, self)
        end

        def key?(type)
          @codecs.key?(type)
        end

        # True when a custom (non built-in) codec claims this Ruby value for
        # encoding, so callers must use its length rule rather than the
        # built-in type table.
        def custom_value?(value)
          !custom_codec_for(value).nil?
        end

        private

        def invoke_decoder(codec, data, offset, type)
          if built_in_type?(type)
            # Built-in nested-table and array decoders need the same codec view.
            codec.decoder.call(data, offset, self)
          else
            wrap_custom_decoder(codec, data, offset)
          end
        end

        # Built-in decoders are trusted and shared by the legacy entry points.
        def built_in_type?(type)
          self.class.built_in_codecs.key?(type)
        end

        def wrap_custom_decoder(codec, data, offset)
          result = codec.decoder.call(data, offset)

          unless result.is_a?(::Array) && result.length == 2
            raise InvalidOffsetError,
                  "custom decoder for type #{codec.type.inspect} must return [value, new_offset], got #{result.inspect}"
          end

          result
        end

        def validate_offset!(new_offset, offset, data_size, type)
          unless new_offset.is_a?(::Integer)
            raise InvalidOffsetError,
                  "custom decoder for type #{type.inspect} reported a non-integer offset #{new_offset.inspect}"
          end

          if new_offset < offset
            raise InvalidOffsetError,
                  "custom decoder for type #{type.inspect} reported offset #{new_offset} that is before the start offset #{offset}"
          end

          if new_offset > data_size
            raise InvalidOffsetError,
                  "custom decoder for type #{type.inspect} reported offset #{new_offset} past the end of the available data (#{data_size})"
          end
        end

        def custom_codec_for(value)
          @codecs.each_value do |codec|
            next if built_in_type?(codec.type)
            next if codec.encoder.nil? || codec.matcher.nil?

            return codec if codec.matcher.call(value)
          end

          nil
        end

        # A length rule that disagrees with the bytes actually produced would
        # corrupt the framing of every later field, so it is diagnosed at once.
        def verify_sizer!(codec, value, encoded)
          return if codec.sizer.nil?

          reported = codec.sizer.call(value)

          unless reported.is_a?(::Integer) && reported >= 0
            raise InvalidOffsetError,
                  "custom sizer for type #{codec.type.inspect} reported an invalid size #{reported.inspect}, expected a non-negative integer"
          end

          if reported != encoded.bytesize
            raise InvalidOffsetError,
                  "custom codec for type #{codec.type.inspect} reported a value length of #{reported} bytes but encoded #{encoded.bytesize} bytes"
          end
        end
      end # CodecSet
    end # Table
  end # Protocol
end # AMQ