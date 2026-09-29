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
    # Server-side session lifetime management is also available, in addition
    # to the client-side cookie expiry controlled by :expire_after:
    # * :absolute_lifetime
    #     Maximum lifetime of a session in seconds, measured from when the
    #     session was first created.
    # * :idle_timeout
    #     Maximum idle time of a session in seconds, measured from the last
    #     activity.
    # * :renewal_threshold
    #     Read-only requests only re-issue the session cookie once this many
    #     seconds have elapsed since the last activity update. Defaults to
    #     half of :idle_timeout.
    # * :clock
    #     A callable returning the current time, used for all server-side
    #     lifetime checks. Defaults to <tt>->{ Time.now }</tt>.
    #
    # The creation and last activity times are stored inside the
    # authenticated (encrypted/signed) cookie content, so a client cannot
    # extend a session by keeping the cookie past its client-side expiry or
    # by rewriting the cookie's expiry attributes. Renewing a session
    # updates the last activity time, but never the creation time.
    #
    # Cookies written before these options were enabled do not contain time
    # metadata. Such legacy cookies are accepted and migrated (stamped with
    # the current time) the next time the session is written; their
    # absolute lifetime is therefore measured from the time of migration.
    #
    # If the server clock moves backwards, elapsed times are clamped to
    # zero: sessions are neither expired nor renewed until the clock catches
    # up with the recorded timestamps.
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

        # Server-side session lifetime management:
        @clock = options.fetch(:clock, ->{ Time.now })
        @absolute_lifetime = options[:absolute_lifetime]
        @idle_timeout = options[:idle_timeout]
        @renewal_threshold = options.fetch(:renewal_threshold) { @idle_timeout && @idle_timeout / 2 }

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

            # Validate server-side timestamps and unwrap the session data:
            session_data = extract_session_data(request, session_data)
          end

          request.set_header(k, session_data || {})
        end
      end

      # Is server-side session lifetime management enabled?
      def track_lifetime?
        @absolute_lifetime || @idle_timeout
      end

      # The current time in seconds since the epoch, according to the
      # configured clock.
      def current_time
        @clock.call.to_i
      end

      # Extract and validate the session data from the decoded cookie
      # payload.
      #
      # When lifetime management is enabled, the payload is an envelope
      # containing the session data along with +created_at+ and
      # +last_activity_at+ timestamps. Payloads without this metadata are
      # legacy cookies: they are accepted as-is and migrated (stamped with
      # fresh timestamps) the next time the session is written.
      def extract_session_data(request, data)
        return data unless track_lifetime? && data.is_a?(Hash)

        session = data["session"]
        created_at = data["created_at"]
        last_activity_at = data["last_activity_at"]

        unless session.is_a?(Hash) && created_at && last_activity_at
          # Legacy cookie without time metadata: accept it, and migrate it
          # to the new format when the session is next written.
          return data
        end

        now = current_time

        if expired_session?(created_at.to_i, last_activity_at.to_i, now)
          request.env[Rack::RACK_ERRORS].puts "Session cookie expired, ignoring it."
          return nil
        end

        # Remember the timestamps and the original session data, so that
        # #write_session can preserve the creation time and detect
        # unmodified (read-only) sessions:
        request.set_header(RACK_SESSION_COOKIE_METADATA, {
          "created_at" => created_at.to_i,
          "last_activity_at" => last_activity_at.to_i,
          "session" => session,
        })

        session
      end

      # Has the session expired according to the server-side timestamps?
      def expired_session?(created_at, last_activity_at, now)
        if @absolute_lifetime && elapsed_time(created_at, now) >= @absolute_lifetime
          true
        elsif @idle_timeout && elapsed_time(last_activity_at, now) >= @idle_timeout
          true
        else
          false
        end
      end

      # The time elapsed since the given timestamp, clamped to zero. If the
      # clock moves backwards (so timestamps appear to be in the future),
      # sessions are neither expired nor renewed until the clock catches up.
      def elapsed_time(timestamp, now)
        elapsed = now - timestamp
        elapsed < 0 ? 0 : elapsed
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

        if track_lifetime?
          now = current_time
          metadata = req.get_header(RACK_SESSION_COOKIE_METADATA)

          created_at = metadata ? metadata["created_at"] : now
          last_activity_at = metadata ? metadata["last_activity_at"] : now

          if metadata && metadata["session"] == session
            # The session was not modified (a read-only request). Only
            # re-issue the cookie once the renewal threshold has been
            # reached; otherwise reuse the original cookie value verbatim,
            # so that no Set-Cookie header is emitted. Renewal updates the
            # last activity time, but never the creation time.
            if @renewal_threshold && elapsed_time(last_activity_at, now) >= @renewal_threshold
              last_activity_at = now
            else
              return SessionId.new(session_id, req.cookies[@key])
            end
          elsif now > last_activity_at
            last_activity_at = now
          end

          session = {
            "session" => session,
            "created_at" => created_at,
            "last_activity_at" => last_activity_at,
          }
        end

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
