# frozen_string_literal: true

module JWT
  # Parses externally received tokens without accepting legacy encodings.
  # @api private
  class StrictTokenParser
    MAX_DECODED_SEGMENT_BYTESIZE = 10 * 1024
    MAX_JSON_NESTING_DEPTH = 10

    ParsedToken = Struct.new(:header, :payload, :signature, keyword_init: true)

    def self.parse(segments)
      new.parse(segments)
    end

    def parse(segments)
      raise MalformedTokenError, 'Not enough or too many segments' unless segments.size == 3

      encoded_header, encoded_payload, encoded_signature = segments
      header = parse_json_segment(:header, encoded_header)
      raise MalformedTokenError, 'Token header not a JSON object' unless header.is_a?(Hash)
      validate_unencoded_payload_header!(header)

      payload = parse_payload(encoded_payload, header)
      signature = decode_base64_segment(:signature, encoded_signature)

      ParsedToken.new(header: header, payload: payload, signature: signature)
    end

    private

    def parse_payload(encoded_payload, header)
      if header['b64'] == false
        check_decoded_size!(:payload, encoded_payload)
        parse_json(:payload, encoded_payload)
      else
        parse_json_segment(:payload, encoded_payload)
      end
    end

    def validate_unencoded_payload_header!(header)
      return unless header['b64'] == false
      return if Array(header['crit']).include?('b64')

      raise InvalidCritError, 'Crit header missing expected values: b64'
    end

    def parse_json_segment(name, segment)
      parse_json(name, decode_base64_segment(name, segment))
    end

    def decode_base64_segment(name, segment)
      check_encoded_size!(name, segment)
      decoded = JWT::Base64.strict_url_decode(segment)
      check_decoded_size!(name, decoded)
      decoded
    end

    def check_encoded_size!(name, segment)
      max_encoded_size = (MAX_DECODED_SEGMENT_BYTESIZE * 4 + 2) / 3
      return if segment.bytesize <= max_encoded_size

      raise MalformedTokenError, "#{name} segment is too large"
    end

    def check_decoded_size!(name, segment)
      return if segment.bytesize <= MAX_DECODED_SEGMENT_BYTESIZE

      raise MalformedTokenError, "#{name} segment is too large"
    end

    def parse_json(name, data)
      JWT::JSON.strict_parse(data, max_nesting: MAX_JSON_NESTING_DEPTH)
    rescue JWT::JSON::DuplicateKeyError
      raise MalformedTokenError, "Duplicate key in #{name}"
    rescue ::JSON::NestingError
      raise MalformedTokenError, "#{name} JSON nesting is too deep"
    rescue ::JSON::ParserError
      raise MalformedTokenError, 'Invalid segment encoding'
    end
  end
end
