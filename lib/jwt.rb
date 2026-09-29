# frozen_string_literal: true

require 'jwt/version'
require 'jwt/base64'
require 'jwt/json'
require 'jwt/decode'
require 'jwt/configuration'
require 'jwt/encode'
require 'jwt/error'
require 'jwt/jwk'
require 'jwt/claims'
require 'jwt/encoded_token'
require 'jwt/token'
require 'jwt/signing_request'

# JSON Web Token implementation
#
# Should be up to date with the latest spec:
# https://tools.ietf.org/html/rfc7519
module JWT
  extend ::JWT::Configuration

  module_function

  # Encodes a payload into a JWT.
  #
  # @param payload [Hash] the payload to encode.
  # @param key [String] the key used to sign the JWT.
  # @param algorithm [String] the algorithm used to sign the JWT.
  # @param header_fields [Hash] additional headers to include in the JWT.
  # @return [String] the encoded JWT.
  def encode(payload, key, algorithm = 'HS256', header_fields = {})
    Encode.new(payload: payload,
               key: key,
               algorithm: algorithm,
               headers: header_fields).segments
  end

  # Decodes a JWT to extract the payload and header
  #
  # @param jwt [String] the JWT to decode.
  # @param key [String] the key used to verify the JWT.
  # @param verify [Boolean] whether to verify the JWT signature.
  # @param options [Hash] additional options for decoding.
  # @return [Array<Hash>] the decoded payload and headers.
  def decode(jwt, key = nil, verify = true, options = {}, &keyfinder) # rubocop:disable Style/OptionalBooleanParameter
    Decode.new(jwt, key, verify, configuration.decode.to_h.merge(options), &keyfinder).decode_segments
  end

  # Prepares a two-step signing request for a key held outside the
  # application. The returned request exposes the bytes to sign and the
  # algorithm requirements; the externally computed signature is handed
  # back via JWT::SigningRequest#complete.
  #
  # @param payload [Hash] the payload to sign.
  # @param algorithm [String, Object] the algorithm the external signer will use.
  # @param header [Hash] additional headers to include in the JWT.
  # @return [JWT::SigningRequest] the prepared signing request.
  def prepare_signature(payload, algorithm:, header: {})
    SigningRequest.new(payload: payload, header: header, algorithm: algorithm)
  end
end
