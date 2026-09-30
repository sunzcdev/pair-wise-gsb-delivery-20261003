# frozen_string_literal: true

module JWT
  # Parses and validates the structure of a compact JWS/JWT before key lookup.
  #
  # @api private
  class StrictTokenParser
    MAX_DECODED_SEGMENT_BYTESIZE = 10 * 1024 * 1024
    MAX_JSON_NESTING_DEPTH = 100

    BASE64URL_PATTERN = /\A[A-Za-z0-9_-]*\z/.freeze

    Result = Struct.new(
      :encoded_header,
      :encoded_payload,
      :encoded_signature,
      :header,
      :payload,
      :signature,
      keyword_init: true
    )

    private_constant(:BASE64URL_PATTERN)

    def self.parse(jwt)
      new(jwt).parse
    end

    def self.parse_payload_segment(segment, header)
      new.parse_payload_segment(segment, header)
    end

    def initialize(jwt = nil)
      @jwt = jwt
    end

    def parse
      encoded_header, encoded_payload, encoded_signature = segments
      header = parse_header_segment(encoded_header)
      payload = parse_payload_segment(encoded_payload, header)
      signature = decode_base64url_segment(encoded_signature)

      Result.new(
        encoded_header: encoded_header,
        encoded_payload: encoded_payload,
        encoded_signature: encoded_signature,
        header: header,
        payload: payload,
        signature: signature
      )
    end

    def parse_header_segment(segment)
      header = parse_json_segment(decode_base64url_segment(segment))

      raise JWT::MalformedTokenError, 'Token header not a JSON object' unless header.is_a?(Hash)

      header
    end

    def parse_payload_segment(segment, header)
      if header['b64'] == false
        check_decoded_size!(segment)
        parse_json(segment)
      else
        parse_json_segment(decode_base64url_segment(segment))
      end
    end

    private

    attr_reader :jwt

    def segments
      parts = jwt.split('.', -1)
      return parts if parts.size == 3

      raise JWT::MalformedTokenError, 'Not enough or too many segments'
    end

    def decode_base64url_segment(segment)
      unless segment.match?(BASE64URL_PATTERN) && segment.bytesize % 4 != 1
        raise JWT::Base64DecodeError, 'Invalid base64 encoding'
      end

      check_encoded_size!(segment)
      decoded = ::Base64.urlsafe_decode64(segment)
      check_decoded_size!(decoded)

      unless ::Base64.urlsafe_encode64(decoded, padding: false) == segment
        raise JWT::Base64DecodeError, 'Invalid base64 encoding'
      end

      decoded
    rescue ArgumentError
      raise JWT::Base64DecodeError, 'Invalid base64 encoding'
    end

    def check_decoded_size!(decoded)
      return if decoded.bytesize <= MAX_DECODED_SEGMENT_BYTESIZE

      raise JWT::MalformedTokenError, 'Decoded segment is too large'
    end

    def check_encoded_size!(segment)
      return if segment.bytesize * 3 / 4 <= MAX_DECODED_SEGMENT_BYTESIZE

      raise JWT::MalformedTokenError, 'Decoded segment is too large'
    end

    def parse_json_segment(decoded_segment)
      check_decoded_size!(decoded_segment)
      parse_json(decoded_segment)
    end

    def parse_json(segment)
      JWT::JSON.parse_strict(segment, max_nesting_depth: MAX_JSON_NESTING_DEPTH)
    rescue JWT::JSON::DuplicateKeyError
      raise JWT::MalformedTokenError, 'Duplicate key in JSON segment'
    rescue ::JSON::NestingError
      raise JWT::MalformedTokenError, 'JSON nesting is too deep'
    rescue ::JSON::ParserError
      raise JWT::MalformedTokenError, 'Invalid segment encoding'
    end
  end
end
