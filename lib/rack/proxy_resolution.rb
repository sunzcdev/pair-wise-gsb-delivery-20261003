# frozen_string_literal: true

module Rack
  # Rack::ProxyResolution describes how a request that passed through
  # one or more reverse proxies was interpreted, given an explicit
  # trusted proxy policy.
  #
  # The resolution is performed hop by hop, starting from the address
  # of the direct client connection (+REMOTE_ADDR+). Forwarding
  # information is only ever adopted from hops that are trusted
  # according to the policy configured for the application instance
  # (via the +rack.request.config+ environment, key
  # <tt>:trusted_proxy</tt>). Untrusted inbound headers never affect
  # the interpretation of the request.
  #
  # Both the RFC 7239 +Forwarded+ header and the de-facto
  # +X-Forwarded-*+ headers are parsed according to their respective
  # protocols, including quoted-string escaping, bracketed IPv6
  # addresses, ports, and multi-element values. If the two header
  # families disagree about the adopted values, neither is used:
  # arbitrarily picking one of two conflicting sources would allow
  # header spoofing, so conflicting headers are discarded and the
  # direct connection values are used instead.
  #
  # Parsing is purely syntactic: no DNS queries are ever performed.
  #
  # The result of a resolution exposes:
  #
  # [+origin+]         the adopted origin (scheme, host and port) of
  #                    the request, as a Rack::ProxyResolution::Origin.
  # [+client_address+] the adopted address of the client, after
  #                    walking the trusted proxy chain.
  # [+hops+]           the addresses of the trusted proxy hops that
  #                    were walked, closest hop first.
  # [+discards+]       a list of Rack::ProxyResolution::Discard
  #                    entries explaining every piece of forwarding
  #                    information that was not adopted, and why.
  #
  # Use Rack::Request#proxy_resolution to resolve a request.
  module ProxyResolution
    # The adopted origin of a request: the scheme, host and port the
    # client used when addressing the (outermost trusted) proxy.
    class Origin
      attr_reader :scheme, :host, :port

      def initialize(scheme:, host:, port:)
        @scheme = scheme
        @host = host
        @port = port
      end

      # The authority form of the origin, with IPv6 hosts wrapped in
      # square brackets, including the port if one was adopted.
      def authority
        return nil unless host = @host

        authority = host.include?(':') ? "[#{host}]" : host
        @port ? "#{authority}:#{@port}" : authority
      end

      # The origin in URI form, e.g. "https://example.org:8443".
      def to_s
        if authority = self.authority
          "#{@scheme}://#{authority}"
        end
      end
    end

    # A single piece of forwarding information that was parsed but not
    # adopted, together with the reason it was discarded:
    #
    # [:untrusted_peer]       the direct peer is not trusted by the
    #                         configured policy, so the header was
    #                         ignored entirely.
    # [:invalid_syntax]       the header value could not be parsed
    #                         according to its protocol.
    # [:disallowed_scheme]    the scheme is not in the allowed scheme
    #                         list and was rejected.
    # [:conflicting_headers]  the +Forwarded+ and +X-Forwarded-*+
    #                         header families disagree; neither was
    #                         adopted.
    # [:unverified_hops]      entries further from the server than the
    #                         adopted client are client-supplied and
    #                         cannot be verified.
    class Discard
      attr_reader :header, :value, :reason

      def initialize(header:, value:, reason:)
        @header = header
        @value = value
        @reason = reason
      end
    end

    # The outcome of resolving a request against the trusted proxy
    # policy of the application instance.
    class Result
      # The adopted origin of the request (Rack::ProxyResolution::Origin).
      attr_reader :origin

      # The adopted address of the client.
      attr_reader :client_address

      # The trusted proxy hops that were walked, starting with the
      # direct connection.
      attr_reader :hops

      # All forwarding information that was discarded, with reasons
      # (Array of Rack::ProxyResolution::Discard).
      attr_reader :discards

      def initialize(origin:, client_address:, hops:, discards:, trusted:)
        @origin = origin
        @client_address = client_address
        @hops = hops.freeze
        @discards = discards.freeze
        @trusted = trusted
      end

      # Whether the direct peer is trusted by the configured policy,
      # and forwarding headers were therefore considered at all.
      def trusted?
        @trusted
      end

      # Whether conflicting +Forwarded+ and +X-Forwarded-*+ headers
      # were detected and discarded.
      def conflict?
        @discards.any?{|discard| discard.reason == :conflicting_headers}
      end
    end
  end
end
