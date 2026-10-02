# frozen_string_literal: true

require 'rack'

# Rack::RelativeRedirect is a simple middleware that converts relative paths in
# redirects in absolute urls, so they conform to RFC2616. It allows the user to
# specify the origin to use (with a sensible default), and resolves every kind
# of relative URI reference per RFC 3986 Section 5:
#
# * "foo/bar"  - a relative-path reference, resolved against the current path
# * "/foo/bar" - an absolute-path reference, resolved against the origin
# * "?q=1"     - a query-only reference, reusing the current path
# * "#frag"    - a fragment-only reference, reusing path and query
# * "//h/p"    - a network-path reference, reusing the current scheme
#
# The current resource path is taken from SCRIPT_NAME + PATH_INFO, so apps
# mounted under a prefix are handled. The path is used verbatim: percent
# encoded octets such as %2F stay inside their path segment instead of being
# decoded into a directory separator.
class Rack::RelativeRedirect
  REDIRECT_STATUSES = [301, 302, 303, 307, 308].freeze
  SCHEME_MAP = {'http'=>'80', 'https'=>'443'}
  # The default proc used if a block is not provided to .new
  # Just uses the url scheme of the request and the server name.
  DEFAULT_ABSOLUTE_PROC = proc do |env, res|
    port = env['SERVER_PORT']
    scheme = env['rack.url_scheme']
    "#{scheme}://#{env['SERVER_NAME']}#{":#{port}" unless SCHEME_MAP[scheme] == port}"
  end

  # Initialize a new RelativeRedirect object with the given arguments.  Arguments:
  # * app : The next middleware in the chain.  This is always called.
  # * &block : If provided, it is called with the environment and the response
  #   from the next middleware. It should return a string representing the scheme
  #   and server name (such as 'http://example.org').
  def initialize(app, &block)
    @app = app
    @absolute_proc = block || DEFAULT_ABSOLUTE_PROC
  end

  # Call the next middleware with the environment.  If the request was a
  # redirect (response status 301, 302, 303, 307 or 308), and the location
  # header is a relative URI reference, resolve it against the origin returned
  # by the block (or the default origin) and the path of the current request.
  # Responses without a Location header, including all non-redirect responses,
  # are passed through untouched and unread.
  def call(env)
    status, headers, body = @app.call(env)

    return [status, headers, body] unless REDIRECT_STATUSES.include?(status)

    # Work on a case-insensitive copy so the app's header object is never
    # mutated, but only keep it when the Location is actually rewritten.
    headers_klass = Rack.release < "3" ? Rack::Utils::HeaderHash : Rack::Headers
    new_headers = headers_klass.new.merge(headers)
    location = new_headers['Location']

    return [status, headers, body] if location.nil? || location.empty?

    # Already an absolute URI, leave it exactly as the app produced it.
    return [status, headers, body] if location =~ %r{\A[a-z][a-z0-9+.\-]*://}i

    response = [status, new_headers, body]
    origin = @absolute_proc.call(env, response)
    base_path = "#{env['SCRIPT_NAME']}#{env['PATH_INFO']}"
    base_query = env['QUERY_STRING']

    resolved = resolve(location, origin, base_path, base_query)

    new_headers['Location'] = resolved

    [status, new_headers, body]
  end

  private

  # Resolve a relative URI reference (RFC 3986 Section 5.3/5.4) without
  # decoding the base path, so %2F remains encoded inside its segment.
  def resolve(location, origin, base_path, base_query)
    if location.start_with?('//')
      scheme = origin[%r{\A([a-z][a-z0-9+.\-]*):}i, 1]
      scheme ? "#{scheme}:#{location}" : location
    elsif location.start_with?('/')
      "#{origin}#{location}"
    elsif location.start_with?('?')
      "#{origin}#{base_path}#{location}"
    elsif location.start_with?('#')
      query = base_query && !base_query.empty? ? "?#{base_query}" : ''
      "#{origin}#{base_path}#{query}#{location}"
    else
      reference_path, reference_query = location.split('?', 2)
      directory = base_path.include?('/') ? base_path[0, base_path.rindex('/')] : ''
      query = reference_query.nil? ? '' : "?#{reference_query}"
      "#{origin}#{directory}/#{reference_path}#{query}"
    end
  end
end
