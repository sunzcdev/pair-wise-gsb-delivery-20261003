# frozen_string_literal: true

module Rack
  class Cors
    # A structured origin rule matching a single, well-formed serialized
    # origin (scheme://host[:port]) instead of relying on a regular
    # expression.
    #
    #   allow do
    #     origins Rack::Cors::Origin.new('https://example.com'),   # exact host
    #             Rack::Cors::Origin.new('https://*.example.com'), # subdomains, full labels
    #             Rack::Cors::Origin.new('https://example.com:8443'),
    #             Rack::Cors::Origin.null                          # the "null" origin
    #     resource '/api/*', headers: :any, methods: :any
    #   end
    #
    # When no port is given, the default port for the scheme is assumed
    # (80 for http, 443 for https), so https://example.com and
    # https://example.com:443 are considered the same origin.
    #
    # Subdomain rules (*.example.com) match whole domain labels only:
    # sub.example.com matches, but example.com itself, evil-example.com,
    # and example.com.evil.test do not.
    #
    # The incoming Origin header must be a single valid origin. Values
    # containing a path, userinfo, or a list of origins never match.
    class Origin
      DEFAULT_PORTS = {
        'http' => 80,
        'https' => 443,
        'ws' => 80,
        'wss' => 443
      }.freeze

      NULL_ORIGIN = 'null'

      # A single serialized origin: scheme://host[:port] with no userinfo,
      # path, query, fragment, or whitespace (which would indicate a list
      # of origins).
      SERIALIZED_ORIGIN = %r{\A([a-z][a-z0-9.+-]*)://(\[[0-9a-f:.]+\]|[^/?#@\s:]+)(?::(\d+))?\z}i

      attr_reader :scheme, :host, :port

      def self.null
        new(NULL_ORIGIN)
      end

      def initialize(definition)
        definition = definition.to_s

        if definition == NULL_ORIGIN
          @null_origin = true
          return
        end

        match = SERIALIZED_ORIGIN.match(definition)
        raise Resource::CorsMisconfigurationError, "Invalid origin rule: #{definition.inspect}" unless match

        @scheme = match[1].downcase

        host = match[2].downcase
        if host.start_with?('*.')
          @subdomains = true
          host = host[2..]
        end
        raise Resource::CorsMisconfigurationError, "Invalid host in origin rule: #{definition.inspect}" if host.empty?

        @host = host
        @port = match[3] ? match[3].to_i : DEFAULT_PORTS[@scheme]
      end

      def null_origin?
        !!@null_origin
      end

      def subdomains?
        !!@subdomains
      end

      def matches?(origin)
        return false unless origin.is_a?(String)
        return origin == NULL_ORIGIN if null_origin?

        match = SERIALIZED_ORIGIN.match(origin)
        return false unless match

        scheme = match[1].downcase
        return false unless scheme == self.scheme

        port = match[3] ? match[3].to_i : DEFAULT_PORTS[scheme]
        return false unless port == self.port

        host_matches?(match[2].downcase)
      end

      protected

      def host_matches?(host)
        if subdomains?
          host.end_with?(".#{self.host}")
        else
          host == self.host
        end
      end
    end
  end
end
