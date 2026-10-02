# frozen_string_literal: true

require 'rack'
require 'uri'

# Rack::RelativeRedirect is a simple middleware that converts relative paths in
# redirects in absolute urls, so they conform to RFC2616. It allows the user to
# specify the absolute path to use (with a sensible default), and handles
# relative paths (those that don't start with a slash) as well.
class Rack::RelativeRedirect
  SCHEME_MAP = {'http'=>'80', 'https'=>'443'}
  REDIRECT_STATUSES = [301, 302, 303, 307, 308].freeze
  ABSOLUTE_URI_REGEXP = %r{\A[A-Za-z][A-Za-z0-9+.-]*:}
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
  # redirect (response status 301, 302, 303, 307, or 308), and the location
  # header is not already an absolute URI, call the block provided by new and
  # use that to make the Location header an absolute url.  Relative references
  # (relative paths, query-only, fragment-only, and network-path references)
  # are resolved against the current request URI following RFC 3986, using
  # SCRIPT_NAME and PATH_INFO as the path of the current resource.  The path
  # is not URL-decoded before resolution, so encoded characters such as %2F
  # stay inside their path segment.
  def call(env)
    status, headers, body = @app.call(env)
    location = relative_redirect_location(status, headers)
    return [status, headers, body] unless location

    headers_klass = Rack.release < "3" ? Rack::Utils::HeaderHash : Rack::Headers
    headers = headers_klass.new.merge(headers)
    headers['Location'] = absolute_location(env, location, [status, headers, body])

    [status, headers, body]
  end

  private

  # Returns the Location header value if the response is a redirect whose
  # Location is a relative reference, nil otherwise.
  def relative_redirect_location(status, headers)
    return nil unless REDIRECT_STATUSES.include?(status)
    key = headers.keys.find { |name| name.to_s.downcase == 'location' }
    location = key && headers[key]
    location if location && !ABSOLUTE_URI_REGEXP.match?(location)
  end

  def absolute_location(env, location, response)
    absolute = @absolute_proc.call(env, response)
    if location.start_with?('/') && !location.start_with?('//')
      "#{absolute}#{location}"
    else
      URI.join(base_uri(env, absolute), location).to_s
    end
  end

  # The absolute URI of the current request, used as the base for resolving
  # relative references.  The path is kept in its encoded form.
  def base_uri(env, absolute)
    base = "#{absolute}#{env['SCRIPT_NAME']}#{env['PATH_INFO']}"
    query = env['QUERY_STRING']
    query.nil? || query.empty? ? base : "#{base}?#{query}"
  end
end
