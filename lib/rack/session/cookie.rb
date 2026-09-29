# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2022-2023, by Samuel Williams.
# Copyright, 2022, by Jeremy Evans.
# Copyright, 2022, by Jon Dufresne.

require 'openssl'
require 'zlib'
require 'json'
require 'base64'
require 'delegate'
require 'time'

require 'rack/constants'
require 'rack/utils'

require_relative 'abstract/id'
require_relative 'encryptor'
require_relative 'constants'

module Rack

  module Session

    # Rack::Session::Cookie provides simple cookie based session management.
    # By default, the session is a Ruby Hash that is serialized and encoded as
    # a cookie set to :key (default: rack.session).
    #
    # This middleware accepts a :secrets option which enables encryption of
    # session cookies. This option should be one or more random "secret keys"
    # that are each at least 64 bytes in length. Multiple secret keys can be
    # supplied in an Array, which is useful when rotating secrets.
    #
    # Several options are also accepted that are passed to Rack::Session::Encryptor.
    # These options include:
    # * :serialize_json
    #     Use JSON for message serialization instead of Marshal. This can be
    #     viewed as a security enhancement.
    # * :gzip_over
    #     For message data over this many bytes, compress it with the deflate
    #     algorithm.
    #
    # Refer to Rack::Session::Encryptor for more details on these options.
    #
    # Prior to version TODO, the session hash was stored as base64 encoded
    # marshalled data. When a :secret option was supplied, the integrity of the
    # encoded data was protected with HMAC-SHA1. This functionality is still
    # supported using a set of a legacy options.
    #
    # Lastly, a :coder option is also accepted. When used, both encryption and
    # the legacy HMAC will be skipped. This option could create security issues
    # in your application!
    #
    # A safe alternative is Rack::Session::Cookie::StrictJSON, which only
    # supports a fixed set of core types (String, true/false, nil, Integer,
    # finite Float, Time, Array, and Hash with String keys) and never
    # instantiates arbitrary classes when decoding.
    #
    # Example:
    #
    #   use Rack::Session::Cookie, {
    #     key: 'rack.session',
    #     domain: 'foo.com',
    #     path: '/',
    #     expire_after: 2592000,
    #     secrets: 'a randomly generated, raw binary string 64 bytes in size',
    #   }
    #
    # Example using legacy HMAC options:
    #
    #   Rack::Session:Cookie.new(application, {
    #     # The secret used for legacy HMAC cookies, this enables the functionality
    #     legacy_hmac_secret: 'legacy secret',
    #     # legacy_hmac_coder will default to Rack::Session::Cookie::Base64::Marshal
    #     legacy_hmac_coder: Rack::Session::Cookie::Identity.new,
    #     # legacy_hmac will default to OpenSSL::Digest::SHA1
    #     legacy_hmac: OpenSSL::Digest::SHA256
    #   })
    #
    # Example of a cookie with no encoding:
    #
    #   Rack::Session::Cookie.new(application, {
    #     :coder => Rack::Session::Cookie::Identity.new
    #   })
    #
    # Example of a cookie with custom encoding:
    #
    #   Rack::Session::Cookie.new(application, {
    #     :coder => Class.new {
    #       def encode(str); str.reverse; end
    #       def decode(str); str.reverse; end
    #     }.new
    #   })
    #

    class Cookie < Abstract::PersistedSecure
      # Encode session cookies as Base64
      class Base64
        def encode(str)
          ::Base64.strict_encode64(str)
        end

        def decode(str)
          ::Base64.decode64(str)
        end

        # Encode session cookies as Marshaled Base64 data
        class Marshal < Base64
          def encode(str)
            super(::Marshal.dump(str))
          end

          def decode(str)
            return unless str
            ::Marshal.load(super(str)) rescue nil
          end
        end

        # N.B. Unlike other encoding methods, the contained objects must be a
        # valid JSON composite type, either a Hash or an Array.
        class JSON < Base64
          def encode(obj)
            super(::JSON.dump(obj))
          end

          def decode(str)
            return unless str
            ::JSON.parse(super(str)) rescue nil
          end
        end

        class ZipJSON < Base64
          def encode(obj)
            super(Zlib::Deflate.deflate(::JSON.dump(obj)))
          end

          def decode(str)
            return unless str
            ::JSON.parse(Zlib::Inflate.inflate(super(str)))
          rescue
            nil
          end
        end
      end

      # Use no encoding for session cookies
      class Identity
        def encode(str); str; end
        def decode(str); str; end
      end

      # A safe session coder which only supports a fixed set of core types:
      # String, true/false, nil, Integer, finite Float, Time, Array, and Hash
      # with String keys. Every value is wrapped in an explicit type tag and
      # the payload carries a version, so the format can evolve safely.
      #
      # Decoding only ever constructs the supported core types: it performs no
      # constant lookup, no Marshal loading, and no arbitrary class callbacks.
      # Duplicate hash keys, unknown type tags, circular structures, and
      # excessively deep nesting all raise deterministic errors.
      #
      # Encoding is atomic: the complete payload is built in memory and is
      # either returned in full or not at all, so a failure can never write a
      # partial session cookie.
      class StrictJSON < Base64
        VERSION = 1
        MAX_DEPTH = 100

        class Error < StandardError; end
        class EncodeError < Error; end
        class DecodeError < Error; end

        def encode(object)
          super(::JSON.dump(
            "version" => VERSION,
            "data" => encode_value(object, 0, {}),
          ))
        end

        def decode(string)
          return unless string

          payload = ::JSON.parse(super(string), max_nesting: false)

          unless payload.is_a?(Hash) && payload["version"] == VERSION && payload.key?("data")
            raise DecodeError, "Unsupported session payload."
          end

          decode_value(payload["data"], 0)
        rescue ::JSON::ParserError => error
          raise DecodeError, error.message
        end

        private

        def encode_value(value, depth, seen)
          if depth > MAX_DEPTH
            raise EncodeError, "Session data is nested too deeply."
          end

          case value
          when nil
            {"type" => "nil"}
          when true, false
            {"type" => "boolean", "value" => value}
          when String
            {"type" => "string", "value" => value}
          when Integer
            {"type" => "integer", "value" => value.to_s}
          when Float
            unless value.finite?
              raise EncodeError, "Cannot encode non-finite Float."
            end
            {"type" => "float", "value" => value.to_s}
          when Time
            {"type" => "time", "value" => value.iso8601(9)}
          when Session::SessionId
            # Session ids are stored as plain strings, matching the
            # behaviour of the JSON-based coders.
            {"type" => "string", "value" => value.to_s}
          when Array
            encode_container(value, seen) do
              {"type" => "array", "value" => value.map{|element| encode_value(element, depth + 1, seen)}}
            end
          when Hash
            encode_container(value, seen) do
              {"type" => "hash", "value" => value.map do |key, element|
                unless key.is_a?(String)
                  raise EncodeError, "Session hash keys must be strings."
                end
                [key, encode_value(element, depth + 1, seen)]
              end}
            end
          else
            raise EncodeError, "Cannot encode #{value.class}."
          end
        end

        def encode_container(value, seen)
          id = value.object_id
          if seen[id]
            raise EncodeError, "Cannot encode circular structure."
          end

          seen[id] = true
          begin
            yield
          ensure
            seen.delete(id)
          end
        end

        def decode_value(node, depth)
          if depth > MAX_DEPTH
            raise DecodeError, "Session data is nested too deeply."
          end

          unless node.is_a?(Hash) && node["type"].is_a?(String)
            raise DecodeError, "Invalid node in session payload."
          end

          case node["type"]
          when "nil"
            nil
          when "boolean"
            value = node["value"]
            unless value == true || value == false
              raise DecodeError, "Invalid boolean in session payload."
            end
            value
          when "string"
            decode_typed(node, String)
          when "integer"
            value = decode_typed(node, String)
            unless value.match?(/\A-?\d+\z/)
              raise DecodeError, "Invalid integer in session payload."
            end
            Integer(value, 10)
          when "float"
            value = decode_typed(node, String)
            float = Float(value, exception: false)
            unless float&.finite?
              raise DecodeError, "Invalid float in session payload."
            end
            float
          when "time"
            value = decode_typed(node, String)
            begin
              Time.iso8601(value)
            rescue ArgumentError
              raise DecodeError, "Invalid time in session payload."
            end
          when "array"
            value = decode_typed(node, Array)
            value.map{|element| decode_value(element, depth + 1)}
          when "hash"
            value = decode_typed(node, Array)
            decode_hash(value, depth)
          else
            raise DecodeError, "Unknown type tag: #{node["type"]}"
          end
        end

        def decode_typed(node, klass)
          value = node["value"]
          unless value.is_a?(klass)
            raise DecodeError, "Invalid #{node["type"]} in session payload."
          end
          value
        end

        def decode_hash(entries, depth)
          hash = {}
          entries.each do |entry|
            unless entry.is_a?(Array) && entry.size == 2 && entry[0].is_a?(String)
              raise DecodeError, "Invalid hash entry in session payload."
            end

            key = entry[0]
            if hash.key?(key)
              raise DecodeError, "Duplicate hash key in session payload: #{key}"
            end

            hash[key] = decode_value(entry[1], depth + 1)
          end
          hash
        end
      end

      class Marshal
        def encode(str)
          ::Marshal.dump(str)
        end

        def decode(str)
          ::Marshal.load(str) if str
        end
      end

      attr_reader :coder, :encryptors

      def initialize(app, options = {})
        # support both :secrets and :secret for backwards compatibility
        secrets = [*(options[:secrets] || options[:secret])]

        encryptor_opts = {
          purpose: options[:key], serialize_json: options[:serialize_json]
        }

        # For each secret, create an Encryptor. We have iterate this Array at
        # decryption time to achieve key rotation.
        @encryptors = secrets.map do |secret|
          Rack::Session::Encryptor.new secret, encryptor_opts
        end

        # If a legacy HMAC secret is present, initialize those features.
        # Fallback to :secret for backwards compatibility.
        if options.has_key?(:legacy_hmac_secret) || options.has_key?(:secret)
          @legacy_hmac = options.fetch(:legacy_hmac, 'SHA1')

          @legacy_hmac_secret = options[:legacy_hmac_secret] || options[:secret]
          @legacy_hmac_coder  = options.fetch(:legacy_hmac_coder, Base64::Marshal.new)
        else
          @legacy_hmac = false
        end

        warn <<-MSG unless secure?(options)
        SECURITY WARNING: No secret option provided to Rack::Session::Cookie.
        This poses a security threat. It is strongly recommended that you
        provide a secret to prevent exploits that may be possible from crafted
        cookies. This will not be supported in future versions of Rack, and
        future versions will even invalidate your existing user cookies.

        Called from: #{caller[0]}.
        MSG

        # Potential danger ahead! Marshal without verification and/or
        # encryption could present a major security issue.
        @coder = options[:coder] ||= Base64::Marshal.new

        super(app, options.merge!(cookie_only: true))
      end

      private

      def find_session(req, sid)
        data = unpacked_cookie_data(req)
        data = persistent_session_id!(data)
        [data["session_id"], data]
      end

      def extract_session_id(request)
        unpacked_cookie_data(request)&.[]("session_id")
      end

      def unpacked_cookie_data(request)
        request.fetch_header(RACK_SESSION_UNPACKED_COOKIE_DATA) do |k|
          if cookie_data = request.cookies[@key]
            session_data = nil

            # Try to decrypt the session data with our encryptors
            encryptors.each do |encryptor|
              begin
                session_data = encryptor.decrypt(cookie_data)
                break
              rescue Rack::Session::Encryptor::Error => error
                request.env[Rack::RACK_ERRORS].puts "Session cookie encryptor error: #{error.message}"

                next
              end
            end

            # If session decryption fails but there is @legacy_hmac_secret
            # defined, attempt legacy HMAC verification
            if !session_data && @legacy_hmac_secret
              # Parse and verify legacy HMAC session cookie
              session_data, _, digest = cookie_data.rpartition('--')
              session_data = nil unless legacy_digest_match?(session_data, digest)

              # Decode using legacy HMAC decoder
              session_data = @legacy_hmac_coder.decode(session_data)

            elsif !session_data && encryptors.empty? && coder
              # Use the coder option, which has the potential to be very unsafe.
              # This path is only reached when no encryptors (secrets:) are configured;
              # if encryptors are present but decryption failed, the cookie is rejected.
              session_data = coder.decode(cookie_data)
            end
          end

          request.set_header(k, session_data || {})
        end
      end

      def persistent_session_id!(data, sid = nil)
        data ||= {}
        data["session_id"] ||= sid || generate_sid
        data
      end

      class SessionId < DelegateClass(Session::SessionId)
        attr_reader :cookie_value

        def initialize(session_id, cookie_value)
          super(session_id)
          @cookie_value = cookie_value
        end
      end

      def write_session(req, session_id, session, options)
        session = session.merge("session_id" => session_id)
        session_data = encode_session_data(session)

        if session_data.size > (4096 - @key.size)
          req.get_header(RACK_ERRORS).puts("Warning! Rack::Session::Cookie data size exceeds 4K.")
          nil
        else
          SessionId.new(session_id, session_data)
        end
      end

      def delete_session(req, session_id, options)
        # Nothing to do here, data is in the client
        generate_sid unless options[:drop]
      end

      def legacy_digest_match?(data, digest)
        return false unless data && digest

        Rack::Utils.secure_compare(digest, legacy_generate_hmac(data))
      end

      def legacy_generate_hmac(data)
        OpenSSL::HMAC.hexdigest(@legacy_hmac, @legacy_hmac_secret, data)
      end

      def encode_session_data(session)
        if encryptors.empty?
          coder.encode(session)
        else
          encryptors.first.encrypt(session)
        end
      end

      # Were consider "secure" if:
      # * Encrypted cookies are enabled and one or more encryptor is
      #   initialized
      # * The legacy HMAC option is enabled
      # * Customer :coder is used, with :let_coder_handle_secure_encoding
      #   set to true
      def secure?(options)
        !@encryptors.empty? ||
          @legacy_hmac ||
          (options[:coder] && options[:let_coder_handle_secure_encoding])
      end
    end
  end
end
