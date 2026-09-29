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
    # == Server-side session expiry
    #
    # In addition to the client-side cookie expiry (:expire_after / :max_age,
    # which the client is free to ignore), the following options enforce
    # server-side session lifetimes. The creation time and last activity time
    # of the session are stored inside the authenticated (encrypted or
    # HMAC-protected) cookie payload, so clients cannot extend a session by
    # modifying cookie attributes or replaying an old cookie:
    #
    # * :absolute_lifetime
    #     Maximum number of seconds a session may live, counted from its
    #     creation time. Once exceeded, the session is discarded regardless
    #     of activity. Renewals (see :renew_after) never reset the creation
    #     time, so this limit cannot be extended by activity.
    # * :idle_timeout
    #     Maximum number of seconds a session may remain unused. Each
    #     request that writes the session slides this window; read-only
    #     requests only slide it once the :renew_after threshold is reached.
    # * :renew_after
    #     Number of seconds of inactivity after which a read-only request
    #     re-issues the cookie with a fresh last-activity time. Below this
    #     threshold, read-only requests do not send a Set-Cookie header at
    #     all. Defaults to half of :idle_timeout. Only meaningful together
    #     with :idle_timeout.
    # * :clock
    #     A callable returning the current time (a Time or epoch seconds),
    #     used for all server-side expiry checks. Defaults to
    #     `proc { Time.now }`. Inject a fake clock in tests.
    #
    # Migration: cookies written before these options were enabled carry no
    # timing metadata. They are still accepted; the server-side clock starts
    # at the first request seen after enabling the options, and the metadata
    # is stamped into the cookie the next time it is re-issued. To forcibly
    # invalidate pre-existing sessions instead, rotate :secrets.
    #
    # Clock rollback: if the clock moves backwards (e.g. NTP correction),
    # negative elapsed time is clamped to zero. Sessions neither expire
    # prematurely nor gain extra lifetime; the timers effectively pause
    # until the clock catches up.
    #

    class Cookie < Abstract::PersistedSecure
      # Reserved key inside the authenticated cookie payload that carries the
      # server-side timing metadata (epoch seconds). It is stripped from the
      # session data before the application sees it.
      TIMING_METADATA_KEY = '__rack.session.timing__'

      # Environment key used to pass the unpacked timing metadata from the
      # request phase to the commit phase.
      RACK_SESSION_TIMING = 'rack.session.cookie.timing'

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

        # Server-side expiry options. See the class documentation for the
        # migration and clock-rollback semantics.
        @absolute_lifetime = options[:absolute_lifetime]
        @idle_timeout = options[:idle_timeout]
        @renew_after = options[:renew_after] || (@idle_timeout && @idle_timeout / 2)
        @clock = options[:clock] || proc { Time.now }

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

            session_data = extract_and_validate_timing_metadata(request, session_data)
          end

          request.set_header(k, session_data || {})
        end
      end

      # Commit the session, but avoid re-issuing the cookie for read-only
      # requests unless the idle-time renewal threshold has been reached.
      def commit_session(req, res)
        session = req.get_header RACK_SESSION

        if @idle_timeout && read_only_session?(session) &&
            !force_options?(session.options) && !renewal_needed?(req)
          # The session was only read and the renewal threshold has not been
          # reached: leave the client-side cookie untouched.
          return
        end

        super
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
        session = session.merge(TIMING_METADATA_KEY => timing_metadata_for_write(req)) if timing_enabled?
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

      def timing_enabled?
        @absolute_lifetime || @idle_timeout
      end

      def clock_now
        @clock.call.to_i
      end

      # Strip the timing metadata from the decoded cookie payload, validate
      # it against the injected clock, and stash it in the environment for
      # the commit phase. Returns the session data the application should
      # see, or nil if the session has expired server-side.
      def extract_and_validate_timing_metadata(request, session_data)
        return session_data unless session_data.is_a?(Hash)

        metadata = session_data.delete(TIMING_METADATA_KEY)
        return session_data unless timing_enabled?

        if metadata.nil?
          # Migration: cookies written before server-side expiry was enabled
          # carry no timing metadata. Accept the session and leave the
          # metadata unset; it will be stamped with the current time when
          # the cookie is next re-issued.
          return session_data
        end

        if timing_metadata_valid?(metadata)
          request.set_header(RACK_SESSION_TIMING, metadata)
          session_data
        else
          request.get_header(RACK_ERRORS).puts("Session cookie expired server-side; discarding session.")
          nil
        end
      end

      def timing_metadata_valid?(metadata)
        created_at = metadata["created_at"]
        last_activity_at = metadata["last_activity_at"]
        return false unless created_at && last_activity_at

        now = clock_now
        # Clock rollback: clamp negative elapsed time to zero so a backwards
        # clock neither expires sessions nor extends their lifetime.
        age = [now - created_at.to_i, 0].max
        idle = [now - last_activity_at.to_i, 0].max

        return false if @absolute_lifetime && age > @absolute_lifetime
        return false if @idle_timeout && idle > @idle_timeout

        true
      end

      # Build the timing metadata for a cookie being written. The creation
      # time is carried over from the existing cookie (renewal never resets
      # it); only the last-activity time is refreshed.
      def timing_metadata_for_write(req)
        now = clock_now
        previous = req.get_header(RACK_SESSION_TIMING) || {}

        {
          "created_at" => previous["created_at"] || now,
          "last_activity_at" => now,
        }
      end

      def read_only_session?(session)
        session.respond_to?(:written?) && session.loaded? && !session.written?
      end

      # Whether a read-only request should re-issue the cookie to slide the
      # idle-timeout window.
      def renewal_needed?(req)
        return false unless @idle_timeout

        metadata = req.get_header(RACK_SESSION_TIMING)
        # Legacy cookie without timing metadata: re-issue once to stamp it.
        return true unless metadata

        clock_now - metadata["last_activity_at"].to_i >= @renew_after
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
