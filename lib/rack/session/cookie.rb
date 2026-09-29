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
    # When the encoded session no longer fits into a single cookie, an
    # explicitly enabled chunked mode can split it over several cookies:
    #
    # * :chunked
    #     Enable chunked session cookies. Accepts +true+ or a Hash with
    #     :max_chunks (default 8) and :max_bytes (the total budget for all
    #     Set-Cookie headers emitted for a single session write, default
    #     4096 * (max_chunks + 1)). Requires :secrets or :legacy_hmac_secret,
    #     which is used to authenticate the chunk manifest.
    #
    # In chunked mode, the main session cookie holds an authenticated manifest
    # ("chunked.<generation>.<count>.<hmac>") and the encoded session data is
    # stored, in order, in generation-tagged chunk cookies named
    # "<key>.<generation>.<index>". The chunks are reassembled in order before
    # being handed to the regular decryptor. Missing, duplicate, oversized or
    # foreign-generation chunks, an out-of-range chunk count, or an invalid
    # manifest signature all reject the entire session. When a session shrinks
    # again, stale chunk cookies are deleted. Every Set-Cookie header written
    # for a session (manifest, chunks and stale chunk deletions) counts toward
    # the :max_bytes budget, so chunking cannot be used to bypass the header
    # size limits.
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
      # Prefix of the manifest value stored in the main session cookie when
      # the session data is split over several chunk cookies.
      CHUNKED_MANIFEST_PREFIX = 'chunked.'

      # Default maximum number of chunk cookies per session.
      CHUNKED_DEFAULT_MAX_CHUNKS = 8

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

        if chunked_options = options[:chunked]
          chunked_options = {} unless chunked_options.is_a?(Hash)

          @chunked_max_chunks = chunked_options.fetch(:max_chunks, CHUNKED_DEFAULT_MAX_CHUNKS)
          @chunked_max_bytes = chunked_options.fetch(:max_bytes, 4096 * (@chunked_max_chunks + 1))

          unless secret = secrets.first || @legacy_hmac_secret
            raise ArgumentError,
              'Rack::Session::Cookie :chunked requires :secrets or :legacy_hmac_secret to authenticate the chunk manifest'
          end

          @chunked_manifest_key = OpenSSL::HMAC.digest(
            OpenSSL::Digest.new('SHA256'), secret, "Rack::Session::Cookie chunk manifest #{@key}"
          )
        end
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
          if cookie_data = read_cookie_data(request)
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

      # Returns the encoded session data for the request. When the main
      # session cookie holds a chunk manifest, the chunk cookies are
      # validated and reassembled in order first. Returns nil when there is
      # no session cookie, or when a chunked session fails validation, in
      # which case the entire session is rejected.
      def read_cookie_data(request)
        return unless cookie_data = request.cookies[@key]

        if cookie_data.start_with?(CHUNKED_MANIFEST_PREFIX)
          reassemble_chunked_session(request, cookie_data)
        else
          cookie_data
        end
      end

      def chunked?
        !@chunked_max_chunks.nil?
      end

      def chunk_name_pattern
        @chunk_name_pattern ||= /\A#{Regexp.escape(@key)}\.([0-9a-f]+)\.(\d+)\z/
      end

      def chunk_manifest_hmac(generation, count)
        OpenSSL::HMAC.hexdigest(
          OpenSSL::Digest.new('SHA256'), @chunked_manifest_key,
          "#{CHUNKED_MANIFEST_PREFIX}#{generation}.#{count}"
        )
      end

      # Extract chunk cookies from the raw Cookie header, mirroring
      # Rack::Utils.parse_cookies_header, so that duplicate chunk cookies
      # (which the parsed cookie Hash would silently merge) can be rejected.
      def extract_chunk_cookies(request)
        chunks = {}
        duplicates = false

        if header = request.get_header('HTTP_COOKIE')
          header.split(/; */).each do |cookie|
            name, value = cookie.split('=', 2)
            next unless name && value

            if (match = chunk_name_pattern.match(name))
              if chunks.key?(name)
                duplicates = true
              else
                chunks[name] = [match[1], match[2].to_i, (Rack::Utils.unescape(value) rescue value)]
              end
            end
          end
        end

        [chunks, duplicates]
      end

      # Validate the manifest and its chunks, and return the reassembled
      # encoded session data. Any validation failure rejects the entire
      # session by returning nil.
      def reassemble_chunked_session(request, manifest)
        errors = request.env[Rack::RACK_ERRORS]

        unless chunked?
          errors.puts 'Session cookie chunk manifest rejected: chunked sessions are not enabled.'
          return nil
        end

        parts = manifest.split('.')
        generation, count, hmac = parts[1], parts[2], parts[3]

        unless parts.size == 4 &&
               generation =~ /\A[0-9a-f]+\z/ &&
               count =~ /\A\d+\z/ &&
               hmac =~ /\A[0-9a-f]{64}\z/
          errors.puts 'Session cookie chunk manifest rejected: malformed manifest.'
          return nil
        end

        count = count.to_i

        unless count.between?(1, @chunked_max_chunks)
          errors.puts "Session cookie chunk manifest rejected: chunk count #{count} is out of range."
          return nil
        end

        unless Rack::Utils.secure_compare(hmac, chunk_manifest_hmac(generation, count))
          errors.puts 'Session cookie chunk manifest rejected: invalid signature.'
          return nil
        end

        chunks, duplicates = extract_chunk_cookies(request)

        if duplicates
          errors.puts 'Session cookie chunks rejected: duplicate chunk cookie.'
          return nil
        end

        unless chunks.all? { |_, (chunk_generation, _, _)| chunk_generation == generation }
          errors.puts 'Session cookie chunks rejected: chunks from other generations present.'
          return nil
        end

        unless chunks.size == count
          errors.puts "Session cookie chunks rejected: expected #{count} chunks, got #{chunks.size}."
          return nil
        end

        data = String.new

        count.times do |index|
          name = "#{@key}.#{generation}.#{index}"

          unless entry = chunks[name]
            errors.puts "Session cookie chunks rejected: missing chunk #{index}."
            return nil
          end

          if entry[2].bytesize > 4096 - name.bytesize
            errors.puts "Session cookie chunks rejected: chunk #{index} exceeds the per-cookie size limit."
            return nil
          end

          data << entry[2]
        end

        data
      end

      # Names of chunk cookies sent by the client which are not part of the
      # given keep list, and should therefore be expired.
      def stale_chunk_names(request, keep = [])
        request.cookies.keys.select { |name| name.match?(chunk_name_pattern) } - keep
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

        if session_data.size <= (4096 - @key.size)
          # The session fits in a single cookie. Expire any stale chunk
          # cookies left over from a previously larger session.
          expire_stale_chunks(req)

          SessionId.new(session_id, session_data)
        elsif chunked?
          write_chunked_session(req, session_id, session_data)
        else
          req.get_header(RACK_ERRORS).puts("Warning! Rack::Session::Cookie data size exceeds 4K.")
          nil
        end
      end

      # Split the encoded session data over generation-tagged chunk cookies,
      # storing an authenticated manifest in the main session cookie. All
      # Set-Cookie headers emitted for the write (manifest, chunks and stale
      # chunk deletions) count toward the header budget; exceeding the chunk
      # count limit or the budget drops the session instead of saving it.
      def write_chunked_session(req, session_id, session_data)
        generation = @sid_secure.hex(8)
        chunks = []
        offset = 0

        while offset < session_data.bytesize
          name = "#{@key}.#{generation}.#{chunks.size}"
          chunk = session_data.byteslice(offset, 4096 - name.bytesize)
          chunks << [name, chunk]
          offset += chunk.bytesize
        end

        if chunks.size > @chunked_max_chunks
          req.get_header(RACK_ERRORS).puts(
            "Warning! Rack::Session::Cookie data size exceeds #{@chunked_max_chunks} chunks."
          )
          return nil
        end

        manifest = "#{CHUNKED_MANIFEST_PREFIX}#{generation}.#{chunks.size}.#{chunk_manifest_hmac(generation, chunks.size)}"

        stale = stale_chunk_names(req, chunks.map(&:first))

        # Every Set-Cookie header written for this session counts toward
        # the header budget.
        budget = @key.bytesize + manifest.bytesize
        chunks.each { |name, value| budget += name.bytesize + value.bytesize }
        stale.each { |name| budget += name.bytesize }

        if budget > @chunked_max_bytes
          req.get_header(RACK_ERRORS).puts(
            "Warning! Rack::Session::Cookie chunked data exceeds the #{@chunked_max_bytes} bytes header budget."
          )
          return nil
        end

        req.set_header(RACK_SESSION_CHUNKED_WRITE, chunks)
        req.set_header(RACK_SESSION_CHUNKED_EXPIRE, stale) unless stale.empty?

        SessionId.new(session_id, manifest)
      end

      # Record any stale chunk cookies from the request, so they can be
      # expired alongside the session cookie.
      def expire_stale_chunks(req, keep = [])
        stale = stale_chunk_names(req, keep)
        req.set_header(RACK_SESSION_CHUNKED_EXPIRE, stale) unless stale.empty?
      end

      def set_cookie(request, response, cookie)
        super

        if chunks = request.get_header(RACK_SESSION_CHUNKED_WRITE)
          chunks.each do |name, value|
            response.set_cookie(name, cookie.merge(value: value))
          end
        end

        if stale = request.get_header(RACK_SESSION_CHUNKED_EXPIRE)
          stale.each do |name|
            response.delete_cookie(name, path: cookie[:path], domain: cookie[:domain])
          end
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
