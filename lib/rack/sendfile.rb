# frozen_string_literal: true

require_relative 'constants'
require_relative 'utils'
require_relative 'body_proxy'

module Rack

  # = Sendfile
  #
  # The Sendfile middleware intercepts responses whose body is being
  # served from a file and replaces it with a server specific x-sendfile
  # header. The web server is then responsible for writing the file contents
  # to the client. This can dramatically reduce the amount of work required
  # by the Ruby backend and takes advantage of the web server's optimized file
  # delivery code.
  #
  # In order to take advantage of this middleware, the response body must
  # respond to +to_path+ and the request must include an `x-sendfile-type`
  # header. Rack::Files and other components implement +to_path+ so there's
  # rarely anything you need to do in your application. The `x-sendfile-type`
  # header is typically set in your web servers configuration. The following
  # sections attempt to document
  #
  # === Nginx
  #
  # Nginx supports the `x-accel-redirect` header. This is similar to `x-sendfile`
  # but requires parts of the filesystem to be mapped into a private URL
  # hierarchy.
  #
  # The following example shows the Nginx configuration required to create
  # a private "/files/" area and enable `x-accel-redirect`:
  #
  #   location ~ /files/(.*) {
  #     internal;
  #     alias /var/www/$1;
  #   }
  #
  #   location / {
  #     proxy_redirect     off;
  #
  #     proxy_set_header   Host                $host;
  #     proxy_set_header   X-Real-IP           $remote_addr;
  #     proxy_set_header   X-Forwarded-For     $proxy_add_x_forwarded_for;
  #
  #     proxy_pass         http://127.0.0.1:8080/;
  #   }
  #
  # To enable `x-accel-redirect`, you must configure the middleware explicitly,
  # together with the mapping plan that translates local file identities into
  # the private (internal redirect) URIs nginx expects:
  #
  #   use Rack::Sendfile, "x-accel-redirect", [["/var/www/", "/files/"]]
  #
  # For security reasons, the `x-sendfile-type` header from requests is ignored.
  # The sendfile variation must be set via the middleware constructor.
  #
  # See Also: https://www.nginx.com/resources/wiki/start/topics/examples/xsendfile
  #
  # === lighttpd
  #
  # Lighttpd has supported some variation of the `x-sendfile` header for some
  # time, although only recent version support `x-sendfile` in a reverse proxy
  # configuration.
  #
  #   $HTTP["host"] == "example.com" {
  #      proxy-core.protocol = "http"
  #      proxy-core.balancer = "round-robin"
  #      proxy-core.backends = (
  #        "127.0.0.1:8000",
  #        "127.0.0.1:8001",
  #        ...
  #      )
  #
  #      proxy-core.allow-x-sendfile = "enable"
  #      proxy-core.rewrite-request = (
  #        "x-sendfile-type" => (".*" => "x-sendfile")
  #      )
  #    }
  #
  # See Also: http://redmine.lighttpd.net/wiki/lighttpd/Docs:ModProxyCore
  #
  # === Apache
  #
  # `x-sendfile` is supported under Apache 2.x using a separate module:
  #
  # https://tn123.org/mod_xsendfile/
  #
  # Once the module is compiled and installed, you can enable it using
  # XSendFile config directive:
  #
  #   RequestHeader Set x-sendfile-type x-sendfile
  #   ProxyPassReverse / http://localhost:8001/
  #   XSendFile on
  #
  # === Mapping plan
  #
  # The third parameter declares an explicit mapping plan. Each entry binds a
  # local filesystem root (the local file identity) to the URI prefix the
  # reverse proxy should redirect to (a public mount point or an internal
  # redirect location, depending on the proxy's configuration):
  #
  #   use Rack::Sendfile, "x-accel-redirect", [["/var/www/", "/files/"]]
  #
  # String roots are treated as literal paths: they are expanded, escaped, and
  # only match at a path boundary, so a root of "/var/www" never maps a file
  # that actually lives under "/var/www2" (adjacent prefixes can never resolve
  # to a different file). A Regexp may be used as the root for advanced cases;
  # it is then the application's responsibility to anchor it safely.
  #
  # Mappings only make sense with the `x-accel-redirect` variation, which
  # carries a URI. The `x-sendfile` and `x-lighttpd-send-file` variations hand
  # the local file identity (the filesystem path itself) to the server
  # verbatim, so combining them with a mapping plan is a configuration error
  # and is rejected.
  #
  # When `x-accel-redirect` is explicitly enabled via the variation parameter
  # and no application-level mapping plan is provided, the middleware will
  # read a comma-separated list of `local_root=uri_prefix` pairs from the
  # `sendfile.mapping` env key. Like `sendfile.type`, this key can only be set
  # by the server or a trusted upstream middleware. The `x-accel-mapping`
  # *request header* is never consulted: configuration supplied by HTTP
  # clients is not a trusted source of mapping plans.
  #
  # === Failure policy
  #
  # When a file-backed body cannot be offloaded (no matching mapping, an
  # unknown variation, ...) the +on_failure+ keyword declares what happens:
  #
  # [:keep (default)] The response is returned untouched: the original body is
  #                   served as-is, its Content-Length stays valid, and body
  #                   ownership remains with the downstream server.
  # [:refuse]         The request is rejected with an empty 500 response. The
  #                   original body is closed (the middleware takes ownership
  #                   to dispose of it) and no Content-Length is inherited.
  #
  # === Security
  #
  # For security reasons, the `x-sendfile-type` header from HTTP requests is
  # ignored. The sendfile variation must be explicitly configured via the
  # middleware constructor to prevent information disclosure vulnerabilities
  # where attackers could bypass proxy restrictions. Likewise, mapping
  # configuration is only ever taken from the constructor or the
  # non-header `sendfile.mapping` env key, never from request headers.
  # Offloading decisions are made using path arithmetic only; the file
  # contents are never read to validate a mapping.

  class Sendfile
    # Env key (never derived from an HTTP header) through which the server or
    # a trusted upstream middleware may supply a comma-separated list of
    # `local_root=uri_prefix` mapping pairs. Mirrors `sendfile.type`.
    SENDFILE_MAPPING = 'sendfile.mapping'

    # Declared policies for bodies that cannot be offloaded. See the
    # "Failure policy" section above.
    ON_FAILURE_POLICIES = %i[keep refuse].freeze

    # A single entry of the explicit mapping plan. Each entry binds a local
    # filesystem root (the local file identity) to the URI prefix (a public
    # mount or an internal redirect location) the reverse proxy should use
    # instead of the path itself.
    class Mapping
      attr_reader :root, :uri_prefix

      def initialize(internal, external)
        if internal.nil? || external.nil?
          raise ArgumentError, "sendfile mappings require both a local root and a URI prefix"
        end

        @uri_prefix = external

        if Regexp === internal
          # Advanced use: the pattern is honored as given. Anchoring and
          # boundary safety are the application's responsibility.
          @root = internal
          @pattern = internal
        else
          # Literal local root: expand, strip any trailing separator, escape,
          # and require the match to end at a path boundary so that adjacent
          # prefixes ("/var/www" vs "/var/www2") can never collide.
          @root = ::File.expand_path(internal).sub(%r{[/\\]+\z}, '')
          @pattern = /\A#{Regexp.escape(@root)}(?=[\/\\]|\z)/i
        end
      end

      # Translates an expanded local path into the mapped URI, or nil when
      # the path does not fall under this entry's local root. Pure path
      # arithmetic: the file itself is never opened or read.
      def translate(path)
        return nil unless match = @pattern.match(path)

        remainder = path[match[0].length..].tr('\\', '/').sub(%r{\A/+}, '')
        if remainder.empty?
          @uri_prefix
        elsif @uri_prefix.end_with?('/')
          "#{@uri_prefix}#{remainder}"
        else
          "#{@uri_prefix}/#{remainder}"
        end
      end
    end

    def initialize(app, variation = nil, mappings = [], on_failure: :keep)
      @app = app
      @variation = variation
      @on_failure = validate_on_failure(on_failure)
      @mappings = build_mapping_plan(mappings)
      verify_scheme_supports_mappings!
    end

    def call(env)
      _, headers, body = response = @app.call(env)

      if body.respond_to?(:to_path)
        case type = variation(env)
        when /x-accel-redirect/i
          # The header carries an internal redirect URI produced by the
          # mapping plan, never the raw local path.
          path = ::File.expand_path(body.to_path)
          if url = map_accel_path(env, path)
            # '?' must be percent-encoded because it is not query string but a part of path
            offload(response, headers, body, type.downcase, ::Rack::Utils.escape_path(url).gsub('?', '%3F'))
          else
            response = offload_failed(env, response, "x-accel-mapping missing or no mapping matched #{path.inspect}")
          end
        when /x-sendfile|x-lighttpd-send-file/i
          # The header carries the local file identity (the path) verbatim;
          # URI mappings cannot be honored by these schemes.
          if @mappings.any?
            response = offload_failed(env, response, "sendfile mappings require x-accel-redirect; #{type.downcase} carries the local path verbatim")
          else
            path = ::File.expand_path(body.to_path)
            offload(response, headers, body, type.downcase, path)
          end
        when '', nil
        else
          response = offload_failed(env, response, "Unknown x-sendfile variation: #{type.inspect}")
        end
      end
      response
    end

    private

    # Hand the body off to the reverse proxy: zero the Content-Length, set
    # the offload header, and transfer ownership of the original body to a
    # proxy wrapper that closes it when the response is done.
    def offload(response, headers, body, header, value)
      headers[CONTENT_LENGTH] = '0'
      headers[header] = value
      obody = body
      response[2] = Rack::BodyProxy.new([]) do
        obody.close if obody.respond_to?(:close)
      end
      response
    end

    # Apply the declared failure policy when a body cannot be offloaded.
    def offload_failed(env, response, reason)
      env[RACK_ERRORS].puts reason

      if @on_failure == :refuse
        # We discard the body, so we own its cleanup; nothing downstream may
        # observe the original Content-Length either.
        _status, _headers, body = response
        body.close if body.respond_to?(:close)
        [500, { CONTENT_TYPE => 'text/plain', CONTENT_LENGTH => '0' }, []]
      else
        # :keep -- return the response untouched: original body, original
        # Content-Length, ownership stays downstream.
        response
      end
    end

    def variation(env)
      # Note: HTTP_X_SENDFILE_TYPE is intentionally NOT read for security reasons.
      # Attackers could use this header to enable x-accel-redirect and bypass proxy restrictions.
      @variation || env['sendfile.type']
    end

    def validate_on_failure(policy)
      policy = policy.to_sym if policy.respond_to?(:to_sym)
      unless ON_FAILURE_POLICIES.include?(policy)
        raise ArgumentError, "unknown on_failure policy: #{policy.inspect} (expected one of #{ON_FAILURE_POLICIES.map(&:inspect).join(', ')})"
      end
      policy
    end

    def build_mapping_plan(mappings)
      mappings.map { |internal, external| Mapping.new(internal, external) }
    end

    # Mappings translate a local file identity into a redirect URI; only the
    # x-accel-redirect scheme carries a URI. Reject mapping plans combined
    # with a scheme that cannot honor them.
    def verify_scheme_supports_mappings!
      return if @mappings.empty?
      return if @variation.nil? # variation chosen per-request; checked at call time
      return if @variation.to_s =~ /x-accel-redirect/i

      raise ArgumentError, "sendfile mappings require the x-accel-redirect variation, not #{@variation.inspect}"
    end

    def map_accel_path(env, path)
      plan = @mappings.empty? ? env_mapping_plan(env) : @mappings
      return nil unless plan

      plan.each do |mapping|
        if url = mapping.translate(path)
          return url
        end
      end
      nil
    end

    def env_mapping_plan(env)
      # Only allow the env-supplied plan when:
      # 1. `x-accel-redirect` is explicitly enabled via constructor.
      # 2. No application-level mappings are configured.
      # Request headers (HTTP_X_ACCEL_MAPPING) are never consulted: mapping
      # configuration from HTTP clients is not trusted.
      return nil unless @variation.to_s =~ /x-accel-redirect/i
      return nil unless raw = env[SENDFILE_MAPPING]

      pairs = raw.split(',').map do |entry|
        internal, external = entry.split('=', 2).map { |part| part&.strip }
        internal.nil? || internal.empty? || external.nil? || external.empty? ? nil : [internal, external]
      end
      build_mapping_plan(pairs.compact)
    end
  end
end
