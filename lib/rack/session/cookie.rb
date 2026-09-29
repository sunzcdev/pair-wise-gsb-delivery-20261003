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
require 'securerandom'
require 'digest/sha2'

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
    # Session data that exceeds the size of a single cookie can be split into
    # multiple chunk cookies by explicitly enabling the :chunked option:
    #
    #   Rack::Session::Cookie.new(application, {
    #     secrets: 'a randomly generated, raw binary string 64 bytes in size',
    #     chunked: true,
    #     # Maximum number of chunk cookies (default: 4):
    #     max_chunks: 4,
    #     # Total byte budget for all Set-Cookie headers written for the
    #     # session, including chunk cookies and stale chunk deletions
    #     # (default: 4096 * (max_chunks + 1)):
    #     header_budget: 20480,
    #   })
    #
    # When chunking is enabled, oversized encrypted session data is split into
    # generation-tagged chunk cookies named "<key>.<generation>.<index>", and
    # the main cookie stores an authenticated manifest for the chunks. The
    # manifest is encrypted with the configured :secrets, so it cannot be forged
    # or modified. Chunks are reassembled in order and verified against the
    # manifest before being handed to the regular decryptor. Missing,
    # duplicated or mixed-generation chunks, manifests exceeding the chunk
    # limit, and assemblies that don't match the manifest digest all reject
    # the entire session. When a session shrinks, stale tail chunks are
    # deleted. Plain single-cookie sessions continue to work unchanged.
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

      class Marshal
        def encode(str)
          ::Marshal.dump(str)
        end

        def decode(str)
          ::Marshal.load(str) if str
        end
      end

      # Default maximum number of chunk cookies a session may be split into.
      DEFAULT_MAX_CHUNKS = 4

      # Env key used to pass the chunk write plan from #write_session to
      # #set_cookie within a single request.
      CHUNK_PLAN_KEY = 'rack.session.cookie.chunk_plan'

      # Env key recording the generation of the incoming chunk manifest,
      # so rewritten chunks can reuse it.
      CHUNK_GENERATION_KEY = 'rack.session.cookie.chunk_generation'

      # Key of the authenticated manifest stored in the main session cookie
      # when the session data is split into chunk cookies.
      CHUNK_MANIFEST_KEY = 'rack.session.chunks'

      # Length (in hexadecimal characters) of chunk generation identifiers.
      CHUNK_GENERATION_LENGTH = 16

      private_constant :CHUNK_PLAN_KEY, :CHUNK_GENERATION_KEY, :CHUNK_MANIFEST_KEY, :CHUNK_GENERATION_LENGTH

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

        initialize_chunking(options)
      end

      private

      # Chunking is disabled unless explicitly enabled with the :chunked
      # option. It requires encrypted cookies (:secrets), as the chunk
      # manifest must be authenticated.
      def initialize_chunking(options)
        @chunked = options[:chunked] || false
        return unless @chunked

        if @encryptors.empty?
          raise ArgumentError, 'the :chunked option requires a :secrets option'
        end

        @max_chunks = options.fetch(:max_chunks, DEFAULT_MAX_CHUNKS)

        unless @max_chunks.is_a?(Integer) && @max_chunks >= 1
          raise ArgumentError, "invalid :max_chunks option: #{@max_chunks.inspect}"
        end

        # Every Set-Cookie header written for the session (the main cookie,
        # chunk cookies and stale chunk deletions) is accounted against this
        # budget, so chunking cannot be used to bypass header size limits.
        @header_budget = options.fetch(:header_budget) { 4096 * (@max_chunks + 1) }

        # The maximum amount of session data stored in a single chunk
        # cookie, accounting for the generation-tagged cookie name.
        @chunk_capacity = 4096 - chunk_cookie_name('0' * CHUNK_GENERATION_LENGTH, @max_chunks - 1).size

        if @chunk_capacity < 1
          raise ArgumentError, 'session cookie :key is too long for chunking'
        end
      end

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
          session_data = nil

          if cookie_data = request.cookies[@key]
            session_data = decrypt_cookie_data(request, cookie_data)

            if @chunked && (manifest = chunk_manifest(session_data))
              # Reuse the incoming generation when the session is
              # rewritten, so unchanged chunks don't need to be resent.
              request.set_header(CHUNK_GENERATION_KEY, manifest.first)
              session_data = load_chunked_session(request, manifest)
            end
          end

          request.set_header(k, session_data || {})
        end
      end

      # Attempt to decrypt/decode the cookie value using the configured
      # encryptors, legacy HMAC or custom coder.
      def decrypt_cookie_data(request, cookie_data)
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

        session_data
      end

      # The name of the chunk cookie storing chunk +index+ of +generation+.
      def chunk_cookie_name(generation, index)
        "#{@key}.#{generation}.#{index}"
      end

      # Matches chunk cookie names belonging to this session cookie,
      # capturing the generation.
      def chunk_cookie_name_pattern
        @chunk_cookie_name_pattern ||= /\A#{Regexp.escape(@key)}\.([0-9a-f]{#{CHUNK_GENERATION_LENGTH}})\.\d+\z/
      end

      # All chunk cookie names present in the raw Cookie request header,
      # including any duplicates.
      def request_chunk_cookie_names(request)
        names = []

        if header = request.get_header('HTTP_COOKIE')
          header.split(/; */n).each do |pair|
            name = pair.split('=', 2).first
            names << name if name =~ chunk_cookie_name_pattern
          end
        end

        names
      end

      # If the decrypted data is an authenticated chunk manifest (rather
      # than regular session data), return it as
      # [generation, count, digest]. The manifest is authenticated because
      # it was encrypted by us with the configured :secrets.
      def chunk_manifest(session_data)
        return unless session_data.is_a?(Hash) && session_data.size == 1

        manifest = session_data[CHUNK_MANIFEST_KEY]
        return unless manifest.is_a?(Hash)

        generation = manifest["generation"]
        count = manifest["count"]
        digest = manifest["digest"]

        return unless generation.is_a?(String) && generation =~ /\A[0-9a-f]{#{CHUNK_GENERATION_LENGTH}}\z/
        return unless count.is_a?(Integer)
        return unless digest.is_a?(String) && digest =~ /\A[0-9a-f]{64}\z/

        [generation, count, digest]
      end

      # Reassemble the encrypted session data from its chunks in order and
      # hand it to the regular decryptor. Any inconsistency (missing,
      # duplicated or mixed-generation chunks, too many chunks, or an
      # assembly that doesn't match the manifest digest) rejects the
      # entire session.
      def load_chunked_session(request, manifest)
        generation, count, digest = manifest

        if count < 1 || count > @max_chunks
          request.get_header(RACK_ERRORS).puts "Session cookie chunk count #{count} exceeds the limit of #{@max_chunks}"
          return nil
        end

        names = request_chunk_cookie_names(request)

        if names.size != names.uniq.size
          request.get_header(RACK_ERRORS).puts "Session cookie contains duplicate chunks"
          return nil
        end

        if names.empty? || names.any? { |name| name[chunk_cookie_name_pattern, 1] != generation }
          request.get_header(RACK_ERRORS).puts "Session cookie chunks do not match the manifest generation"
          return nil
        end

        chunks = (0...count).map do |index|
          unless chunk = request.cookies[chunk_cookie_name(generation, index)]
            request.get_header(RACK_ERRORS).puts "Session cookie chunk #{index} is missing"
            return nil
          end

          chunk
        end

        assembled = chunks.join

        unless Rack::Utils.secure_compare(Digest::SHA256.hexdigest(assembled), digest)
          request.get_header(RACK_ERRORS).puts "Session cookie chunks do not match the manifest digest"
          return nil
        end

        decrypt_cookie_data(request, assembled)
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
          prepare_chunk_plan(req, nil) if @chunked
          SessionId.new(session_id, session_data)
        elsif @chunked
          write_chunked_session(req, session_id, session_data)
        else
          req.get_header(RACK_ERRORS).puts("Warning! Rack::Session::Cookie data size exceeds 4K.")
          nil
        end
      end

      # Split the encrypted session data into generation-tagged chunks and
      # use an authenticated manifest as the main cookie value, describing
      # how the chunks must be reassembled.
      def write_chunked_session(req, session_id, session_data)
        generation = req.get_header(CHUNK_GENERATION_KEY) || SecureRandom.hex(CHUNK_GENERATION_LENGTH / 2)

        chunks = []
        offset = 0
        while offset < session_data.bytesize
          chunks << session_data.byteslice(offset, @chunk_capacity)
          offset += @chunk_capacity
        end

        if chunks.size > @max_chunks
          req.get_header(RACK_ERRORS).puts("Warning! Rack::Session::Cookie data size exceeds #{@max_chunks} chunks.")
          return nil
        end

        manifest = {
          CHUNK_MANIFEST_KEY => {
            "generation" => generation,
            "count" => chunks.size,
            "digest" => Digest::SHA256.hexdigest(session_data)
          }
        }

        prepare_chunk_plan(req, "generation" => generation, "chunks" => chunks)

        SessionId.new(session_id, encryptors.first.encrypt(manifest))
      end

      # Record which chunk cookies must be written and which stale chunk
      # cookies (e.g. left over when a session shrinks) must be deleted
      # when the session is committed.
      def prepare_chunk_plan(req, plan)
        written = []

        if plan && chunks = plan["chunks"]
          written = chunks.each_index.map do |index|
            chunk_cookie_name(plan["generation"], index)
          end
        end

        stale = request_chunk_cookie_names(req).uniq - written

        req.set_header(CHUNK_PLAN_KEY, (plan || {}).merge("stale" => stale))
      end

      # Sets the session cookie back to the client. When chunking is
      # enabled, all Set-Cookie headers written for the session (the main
      # cookie, chunk cookies and stale chunk deletions) are accounted
      # against the header budget.
      def set_cookie(request, response, cookie)
        plan = request.get_header(CHUNK_PLAN_KEY)
        return super unless plan

        headers = [Rack::Utils.set_cookie_header(@key, cookie)]

        if chunks = plan["chunks"]
          chunks.each_with_index do |chunk, index|
            headers << Rack::Utils.set_cookie_header(
              chunk_cookie_name(plan["generation"], index),
              cookie.merge(value: chunk)
            )
          end
        end

        delete_options = { path: cookie[:path], domain: cookie[:domain] }
        plan["stale"].each do |name|
          headers << Rack::Utils.delete_set_cookie_header(name, delete_options)
        end

        if headers.sum(&:bytesize) > @header_budget
          request.get_header(RACK_ERRORS).puts("Warning! Rack::Session::Cookie Set-Cookie headers exceed the #{@header_budget} byte budget. Content dropped.")
          return
        end

        headers.each do |header|
          response.add_header(Rack::SET_COOKIE, header)
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
