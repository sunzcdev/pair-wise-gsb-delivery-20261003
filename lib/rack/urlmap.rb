# frozen_string_literal: true

require 'set'

require_relative 'constants'

module Rack
  # Rack::URLMap takes a hash mapping urls or paths to apps, and
  # dispatches accordingly.  Support for HTTP/1.1 host names exists if
  # the URLs start with <tt>http://</tt> or <tt>https://</tt>.
  #
  # URLMap modifies the SCRIPT_NAME and PATH_INFO such that the part
  # relevant for dispatch is in the SCRIPT_NAME, and the rest in the
  # PATH_INFO.  This should be taken care of when you need to
  # reconstruct the URL in order to create links.
  #
  # URLMap dispatches in such a way that the longest paths are tried
  # first, since they are most specific.
  #
  # Host names are matched case-insensitively, and a default port
  # (80 for http, 443 for https) is ignored on both the mapped
  # locations and the request host, so that e.g. a mapping of
  # <tt>http://example.org/</tt> also matches <tt>example.org:80</tt>.
  # If no mapped host matches the request host, requests fall back to
  # the first mapping without a specified host.
  #
  # The routing table can be swapped at runtime with #remap.  The whole
  # new table (both the path mappings and the set of known hosts) is
  # validated before it is installed, so a failed remap leaves the
  # previously active table untouched.  Each request dispatches against
  # the snapshot of the table that was active when the request started
  # matching, and never observes a table swapped while it is running.

  class URLMap
    def initialize(map = {})
      remap(map)
    end

    # Replace the whole routing table in a single atomic step.
    #
    # Every entry of +map+ is validated and compiled into local
    # structures first; only when the entire table is valid are the
    # instance variables swapped, so an invalid entry never leaves the
    # URLMap with a partially updated mapping or known-hosts set.
    def remap(map)
      known_hosts = Set[]
      mapping = map.map { |location, app|
        scheme = nil

        if location =~ %r{\A(https?)://(.*?)(/.*)}
          scheme = $1
          host, location = $2, $3
          known_hosts << normalize_host(host, scheme)
        else
          host = nil
        end

        unless location[0] == ?/
          raise ArgumentError, "paths need to start with /"
        end

        location = location.chomp('/')
        match = Regexp.new("\\A#{Regexp.quote(location).gsub('/', '/+')}(.*)", Regexp::NOENCODING).freeze

        # Store the normalized host so all host comparisons below are
        # case-insensitive and ignore default ports.
        [normalize_host(host, scheme), location, match, app]
      }.sort_by do |(host, location, _, _)|
        [host ? -host.size : Float::INFINITY, -location.size]
      end.freeze

      # Swap both pieces of state together only after the complete new
      # table has been built successfully; on failure the old table is
      # left in place because the instance variables were never touched.
      @known_hosts = known_hosts.freeze
      @mapping = mapping
    end

    def call(env)
      path        = env[PATH_INFO]
      script_name = env[SCRIPT_NAME]
      http_host   = env[HTTP_HOST]
      server_name = env[SERVER_NAME]
      server_port = env[SERVER_PORT]

      # Snapshot the routing table for this request.  A #remap happening
      # during this call (in another thread, or re-entrantly from an
      # app) swaps the instance variables but cannot mix another version
      # of the table into this request's matching.
      mapping     = @mapping
      known_hosts = @known_hosts

      scheme = env[RACK_URL_SCHEME]
      normalized_http_host   = normalize_host(http_host, scheme)
      normalized_server_name = normalize_host(server_name, scheme)

      is_same_server = casecmp?(normalized_http_host, normalized_server_name) ||
                       casecmp?(normalized_http_host, normalize_host("#{server_name}:#{server_port}", scheme))

      # If we don't have a matching host, default to mappings without a
      # specified host.  Lookup is normalized so host case and default
      # ports agree with the actual host comparisons below.
      is_host_known = known_hosts.include?(normalized_http_host)

      mapping.each do |host, location, match, app|
        unless casecmp?(normalized_http_host, host) \
            || (http_host.nil? && casecmp?(normalized_server_name, host)) \
            || (!host && is_same_server) \
            || (!host && !is_host_known)
          next
        end

        next unless m = match.match(path.to_s)

        rest = m[1]
        next unless !rest || rest.empty? || rest[0] == ?/

        env[SCRIPT_NAME] = (script_name + location)
        env[PATH_INFO] = rest

        return app.call(env)
      end

      [404, { CONTENT_TYPE => "text/plain", "x-cascade" => "pass" }, ["Not Found: #{path}"]]

    ensure
      # Always restore the original dispatch variables.  This matters
      # when URLMaps are nested or when an app raises: the remainder of
      # one request's path must never leak into another request handled
      # by the same environment hash.
      env[PATH_INFO]   = path
      env[SCRIPT_NAME] = script_name
    end

    private
    def casecmp?(v1, v2)
      # if both nil, or they're the same string
      return true if v1 == v2

      # if either are nil... (but they're not the same)
      return false if v1.nil?
      return false if v2.nil?

      # otherwise check they're not case-insensitive the same
      v1.casecmp(v2).zero?
    end

    # Downcase a host and strip the default port for the given scheme
    # ("http" => 80, "https" => 443).  Returns nil for a nil host.
    # When +scheme+ is nil, either default port is stripped, which keeps
    # scheme-less comparisons independent of the port convention.
    def normalize_host(host, scheme = nil)
      return nil if host.nil?

      hostname, port = host.downcase.split(':', 2)
      if port && (port == "80" && scheme != "https" || port == "443" && scheme != "http")
        hostname
      elsif port
        "#{hostname}:#{port}"
      else
        hostname
      end
    end
  end
end
