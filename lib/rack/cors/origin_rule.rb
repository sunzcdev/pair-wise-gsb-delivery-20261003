# frozen_string_literal: true

require 'uri'

module Rack
  class Cors
    # A structured origin allow-list rule that matches the Origin header by
    # scheme, host, and port instead of a single broad regular expression.
    #
    #   origins Rack::Cors::OriginRule.exact('example.com', schemes: %w[https])
    #   origins Rack::Cors::OriginRule.subdomains('example.com')
    #   origins Rack::Cors::OriginRule.null
    #
    # The Origin header must contain a single, well-formed origin: values
    # with a path, userinfo, query, fragment, or multiple origins never
    # match. An origin without an explicit port is matched against the
    # default port of its scheme, so "https://example.com" and
    # "https://example.com:443" are equivalent.
    class OriginRule
      DEFAULT_PORTS = {
        'http' => 80,
        'https' => 443,
        'ws' => 80,
        'wss' => 443
      }.freeze

      DEFAULT_SCHEMES = %w[http https].freeze

      # Matches exactly the given host (no subdomains).
      def self.exact(host, schemes: DEFAULT_SCHEMES, ports: nil)
        new(host, schemes: schemes, ports: ports, subdomains: false)
      end

      # Matches full domain labels below the given host: a rule for
      # "example.com" matches "app.example.com" but not "example.com"
      # (unless include_root: true), "evil-example.com", or
      # "example.com.evil".
      def self.subdomains(host, schemes: DEFAULT_SCHEMES, ports: nil, include_root: false)
        new(host, schemes: schemes, ports: ports, subdomains: true, include_root: include_root)
      end

      # Matches the "null" origin (e.g. sandboxed iframes). Must be declared
      # explicitly; no other rule matches it.
      def self.null
        NullOriginRule.new
      end

      def initialize(host, schemes: DEFAULT_SCHEMES, ports: nil, subdomains: false, include_root: false)
        @host = validate_host(host)
        @schemes = [schemes].flatten.map { |scheme| scheme.to_s.downcase }.freeze
        @ports = ports && [ports].flatten.map(&:to_i).freeze
        @subdomains = subdomains
        @include_root = include_root
      end

      def match?(origin)
        uri = parse_origin(origin)
        return false if uri.nil?
        return false unless @schemes.include?(uri.scheme.downcase)
        return false unless host_matches?(uri.host)

        port_matches?(uri)
      end

      protected

      def validate_host(host)
        host = host.to_s.downcase
        unless host.match?(/\A[a-z0-9]+([.-][a-z0-9]+)*\z/)
          raise ArgumentError, "invalid host for origin rule: #{host.inspect}"
        end

        host
      end

      def parse_origin(origin)
        return nil unless origin.is_a?(String)
        return nil if origin.empty? || origin.match?(/\s/)

        uri = URI.parse(origin)
        return nil if uri.scheme.nil? || uri.host.nil? || uri.host.empty?
        return nil unless uri.userinfo.nil? && uri.opaque.nil?
        return nil unless uri.path.nil? || uri.path.empty?
        return nil unless uri.query.nil? && uri.fragment.nil?

        uri
      rescue URI::InvalidURIError
        nil
      end

      def host_matches?(host)
        host = host.downcase
        if @subdomains
          return true if @include_root && host == @host

          host.end_with?(".#{@host}")
        else
          host == @host
        end
      end

      def port_matches?(uri)
        scheme = uri.scheme.downcase
        origin_port = uri.port || DEFAULT_PORTS[scheme]

        if @ports
          @ports.include?(origin_port)
        elsif DEFAULT_PORTS.key?(scheme)
          origin_port == DEFAULT_PORTS[scheme]
        else
          origin_port.nil?
        end
      end

      # Matches only the "null" origin. See OriginRule.null.
      class NullOriginRule < OriginRule
        def initialize
        end

        def match?(origin)
          origin == 'null'
        end
      end
    end
  end
end
