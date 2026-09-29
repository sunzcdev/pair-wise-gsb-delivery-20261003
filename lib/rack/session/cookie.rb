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
    # Session revocation ("log out of all devices") is supported through the
    # :revocation_check option. The cookie carries a subject identifier (stored
    # in the session under REVOCATION_SUBJECT_KEY) and the epoch at which the
    # session was issued (stored under REVOCATION_EPOCH_KEY, managed by this
    # middleware). On every request the current epoch for the subject is
    # resolved by calling the :revocation_check proc with the subject; if it
    # does not match the epoch stored in the cookie, the session is rejected.
    # The proc is expected to consult your own datastore (a database client is
    # deliberately not included) and return the current epoch, e.g. an integer
    # that you increment to revoke all sessions for a subject:
    #
    #   Rack::Session::Cookie.new(application, {
    #     secrets: secret,
    #     revocation_check: ->(subject) { SessionEpochs.fetch(subject) },
    #   })
    #
    # Behavior is specified as follows:
    # * Anonymous sessions (no subject stored in the session) are never
    #   checked and the revocation_check proc is not called.
    # * If the subject changes during a request (subject switching), the new
    #   subject is checked when the session is written, and the cookie is
    #   stamped with the new subject's current epoch.
    # * If the revocation_check proc raises or returns nil (callback
    #   unavailable), the middleware fails closed: the session is rejected on
    #   read and not saved on write.
    # * Within a single request, the proc is called at most once per subject;
    #   the result is memoized in the request environment.
    # * The epoch stored in the cookie is always re-stamped from the
    #   revocation_check proc when the session is written, never copied from
    #   the incoming cookie. A cookie issued before an epoch bump therefore
    #   cannot become valid again through session renewal.
    #

    class Cookie < Abstract::PersistedSecure
      # Session key holding the subject identifier used for revocation checks.
      # Applications set this (e.g. to the user id) to enroll a session in
      # revocation checking.
      REVOCATION_SUBJECT_KEY = "_revocation_subject"

      # Session key holding the epoch at which the session was issued. This is
      # managed by the middleware and overwritten on every write; applications
      # should not set it directly.
      REVOCATION_EPOCH_KEY = "_revocation_epoch"

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
        @revocation_check = options.delete(:revocation_check)

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

          request.set_header(k, revoked(request, session_data) ? {} : session_data || {})
        end
      end

      # Is the unpacked session data revoked? Always false when no
      # :revocation_check is configured or the session is anonymous (carries
      # no subject identifier).
      def revoked(request, session_data)
        return false unless @revocation_check
        return false unless session_data

        subject = session_data[REVOCATION_SUBJECT_KEY]
        return false unless subject

        current_epoch = current_revocation_epoch(request, subject)

        if current_epoch.nil?
          request.env[Rack::RACK_ERRORS].puts "Session revocation check unavailable for subject #{subject.inspect}; rejecting session."
          return true
        end

        if session_data[REVOCATION_EPOCH_KEY] != current_epoch
          request.env[Rack::RACK_ERRORS].puts "Session revoked for subject #{subject.inspect}: issued at epoch #{session_data[REVOCATION_EPOCH_KEY].inspect}, current epoch is #{current_epoch.inspect}."
          return true
        end

        false
      end

      # Resolve the current revocation epoch for a subject, calling the
      # :revocation_check proc at most once per subject per request. Returns
      # nil when the proc is unavailable (raised or returned nil).
      def current_revocation_epoch(request, subject)
        epochs = request.get_header(RACK_SESSION_REVOCATION_EPOCHS)
        unless epochs
          epochs = {}
          request.set_header(RACK_SESSION_REVOCATION_EPOCHS, epochs)
        end

        unless epochs.key?(subject)
          epochs[subject] = begin
            @revocation_check.call(subject)
          rescue => error
            request.env[Rack::RACK_ERRORS].puts "Session revocation check failed for subject #{subject.inspect}: #{error.message}"
            nil
          end
        end

        epochs[subject]
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
        if @revocation_check
          session = session.dup
          subject = session[REVOCATION_SUBJECT_KEY]

          if subject
            # Always re-stamp the epoch from the revocation check, never from
            # the incoming cookie, so a cookie issued before an epoch bump
            # cannot be renewed back into validity.
            current_epoch = current_revocation_epoch(req, subject)
            return nil unless current_epoch

            session[REVOCATION_EPOCH_KEY] = current_epoch
          else
            session.delete(REVOCATION_EPOCH_KEY)
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
