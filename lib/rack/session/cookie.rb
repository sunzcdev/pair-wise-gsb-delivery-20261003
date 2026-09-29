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
    # Session revocation ("log out of all devices"):
    #
    # The :revocation option adds a small revocation interface on top of the
    # encrypted cookie. The cookie additionally carries only two pieces of
    # metadata: a subject identifier (taken from the session) and the epoch
    # at which the session was issued. The application supplies a callback
    # which returns the current epoch for a subject; if it does not match
    # the epoch stored in the cookie, the session is rejected. Bumping the
    # epoch (e.g. in your own database) therefore invalidates all previously
    # issued cookies for that subject. The library does not ship any database
    # client: where and how epochs are stored is entirely up to the
    # application.
    #
    #   Rack::Session::Cookie.new(application, {
    #     secrets: 'a randomly generated, raw binary string 64 bytes in size',
    #     revocation: {
    #       # Where to find the subject identifier in the session. May be a
    #       # session key (String/Symbol) or a callable receiving the session
    #       # data and returning the subject (or nil for anonymous sessions).
    #       subject: "user_id",
    #       # Required callable returning the current epoch for a subject.
    #       # Epochs should be simple values (e.g. Integer or String) so they
    #       # survive serialization. Returning nil or raising is treated as
    #       # "epoch unavailable" and fails closed.
    #       epoch: ->(subject) { UserEpochs.fetch(subject) },
    #     },
    #   })
    #
    # Behaviour:
    # * Anonymous sessions (no subject) are not revocable: the callback is
    #   never invoked for them and no epoch is stored in their cookie.
    # * The callback is invoked at most once per subject per request; the
    #   result (including failures) is memoized in the request environment,
    #   so repeated session reads within one request do not repeat the call.
    # * If the callback raises or returns nil, the check fails closed: on
    #   read the session is rejected, on write the session is not persisted.
    # * When the subject changes within a request (e.g. logging in as a
    #   different user), the epoch is resolved freshly for the new subject
    #   and stored in the newly issued cookie.
    # * A cookie issued before an epoch bump can never become valid again:
    #   it is rejected on read, so its stale epoch is never copied into a
    #   renewed cookie. Renewing simply issues a fresh anonymous cookie.
    #

    class Cookie < Abstract::PersistedSecure
      # Session key under which the issuing epoch is stored.
      SESSION_EPOCH_KEY = "session_epoch"

      # Sentinel for "the current epoch could not be resolved".
      UNKNOWN_EPOCH = Object.new
      private_constant :UNKNOWN_EPOCH

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

        # Optional revocation interface. Removed from the options so the
        # callables are not leaked into the per-request session options.
        if revocation = options.delete(:revocation)
          @revocation_subject = revocation.fetch(:subject)
          @revocation_epoch = revocation.fetch(:epoch)

          unless @revocation_epoch.respond_to?(:call)
            raise ArgumentError, ":revocation :epoch must respond to #call"
          end
        end

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

            session_data = check_revocation(request, session_data) if session_data
          end

          request.set_header(k, session_data || {})
        end
      end

      # Validate the unpacked session data against the current revocation
      # epoch. Returns the session data if it is valid, or nil if the
      # session must be rejected. Called at most once per request, as the
      # caller memoizes the result in the request environment.
      def check_revocation(request, session_data)
        return session_data unless @revocation_epoch
        return session_data unless session_data.is_a?(Hash)

        subject = revocation_subject(session_data)

        # Anonymous sessions carry no subject and are not revocable; the
        # epoch callback is not invoked for them.
        return session_data if subject.nil?

        epoch = resolve_revocation_epoch(request, subject)

        if epoch == UNKNOWN_EPOCH
          request.env[Rack::RACK_ERRORS].puts "Session rejected: revocation epoch unavailable for subject #{subject.inspect}."
          nil
        elsif epoch != session_data[SESSION_EPOCH_KEY]
          request.env[Rack::RACK_ERRORS].puts "Session rejected: stale revocation epoch for subject #{subject.inspect}."
          nil
        else
          session_data
        end
      end

      # Extract the subject identifier from the session data.
      def revocation_subject(session_data)
        if @revocation_subject.respond_to?(:call)
          @revocation_subject.call(session_data)
        else
          session_data[@revocation_subject.to_s]
        end
      end

      # Resolve the current epoch for a subject via the application
      # callback. Results (including failures) are memoized in the request
      # environment, so the callback runs at most once per subject per
      # request, no matter how often the session is read or written.
      def resolve_revocation_epoch(request, subject)
        cache = request.get_header(RACK_SESSION_REVOCATION_EPOCHS)
        unless cache
          cache = {}
          request.set_header(RACK_SESSION_REVOCATION_EPOCHS, cache)
        end

        if cache.key?(subject)
          cache[subject]
        else
          cache[subject] = fetch_revocation_epoch(request, subject)
        end
      end

      def fetch_revocation_epoch(request, subject)
        epoch = @revocation_epoch.call(subject)

        # A nil epoch is treated as unavailable and fails closed.
        epoch.nil? ? UNKNOWN_EPOCH : epoch
      rescue => error
        request.env[Rack::RACK_ERRORS].puts "Session revocation epoch callback failed: #{error.message}"
        UNKNOWN_EPOCH
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
        if @revocation_epoch
          subject = revocation_subject(session)

          if subject.nil?
            # Anonymous sessions do not carry a revocation epoch.
            session = session.dup
            session.delete(SESSION_EPOCH_KEY)
          else
            epoch = resolve_revocation_epoch(req, subject)

            # Fail closed: without a current epoch we cannot issue a valid
            # authenticated cookie, so the session is not persisted.
            return false if epoch == UNKNOWN_EPOCH

            # Always stamp the *current* epoch, never one copied from an
            # incoming cookie, so a revoked cookie cannot be renewed back
            # into validity.
            session = session.merge(SESSION_EPOCH_KEY => epoch)
          end
        end

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
