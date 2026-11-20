require_relative "spec_helper"

# Demonstrates a two-level delegation where the outer Roda app first
# rewrites one path segment by rule (path_rewriter), then hands the
# remaining path to a Rack sub-app (r.run).  Each request carries
# readable routing coordinates in env['coord.trace']:
#
#   * the original path (and query string) at entry,
#   * the matched prefix at each delegation layer,
#   * the final remaining path handed to the sub-app.
#
# r.run saves/restores SCRIPT_NAME/PATH_INFO around the sub-app call
# (ensure), both on normal return and when the sub-app raises, so the
# outer error_handler observes the original env coordinates, while the
# sub-app observes the delegated coordinates.  QUERY_STRING is never
# touched and passes through unchanged.
describe "routing coordinates for a rewritten, two-level delegated path" do
  # Middleware that makes the coordinates recorded in env readable from
  # outside: it mirrors them into a response header, and records the
  # SCRIPT_NAME/PATH_INFO it observes after the app returns or raises.
  class CoordProbe
    def initialize(app)
      @app = app
    end

    def call(env)
      status, headers, body = @app.call(env)
      if (trace = env['coord.trace'])
        headers['x-route-trace'] = trace.map{|t| t.join('=')}.join(' | ')
      end
      [status, headers, body]
    ensure
      env['coord.after'] = [env['SCRIPT_NAME'], env['PATH_INFO']]
    end
  end

  before do
    inner = lambda do |env|
      # What the innermost Rack app observes.
      env['coord.inner'] = [env['SCRIPT_NAME'], env['PATH_INFO'], env['QUERY_STRING']]
      raise "inner boom" if env['PATH_INFO'] == '/boom'
      [200, {'content-type' => 'text/plain'}, ["inner ok"]]
    end

    middle = Class.new(Roda) do
      route do |r|
        r.on 'v1' do
          r.env['coord.trace'] << ['middle matched', r.matched_path]
          r.env['coord.trace'] << ['middle delegates', r.remaining_path]
          r.run inner
        end
      end
    end

    app(:bare) do
      use CoordProbe

      plugin :path_rewriter
      # Rewrite one segment by rule before any routing happens.  Only
      # remaining_path is rewritten; PATH_INFO keeps the original path.
      rewrite_path '/legacy', '/api'

      plugin :error_handler do |e|
        "outer error: #{e.message} " \
          "script_name=#{request.script_name.inspect} " \
          "path_info=#{request.path_info.inspect} " \
          "remaining_path=#{request.remaining_path.inspect}"
      end

      route do |r|
        r.env['coord.trace'] = [
          ['entry path', r.path],
          ['entry remaining', r.remaining_path],
          ['entry query', r.env['QUERY_STRING']],
        ]
        r.on 'api' do
          r.env['coord.trace'] << ['outer matched', r.matched_path]
          r.env['coord.trace'] << ['outer delegates', r.remaining_path]
          r.run middle
        end
      end
    end
  end

  def call_env(path, query)
    env = req_env('PATH_INFO' => path, 'SCRIPT_NAME' => '', 'QUERY_STRING' => query)
    [@app.call(env), env]
  end

  it "exposes original path, per-layer matched prefixes, and delegated path on success" do
    (status, headers, body), env = call_env('/legacy/v1/items', 'x=1&y=2')

    status.must_equal 200
    b = +''; body.each{|s| b << s}; b.must_equal 'inner ok'

    trace = env['coord.trace']
    # Original path and rewritten remaining path at entry; query untouched.
    trace.must_equal [
      ['entry path', '/legacy/v1/items'],
      ['entry remaining', '/api/v1/items'],
      ['entry query', 'x=1&y=2'],
      ['outer matched', '/legacy'],
      ['outer delegates', '/v1/items'],
      ['middle matched', '/legacy/v1'],
      ['middle delegates', '/items'],
    ]

    # Coordinates are also readable from the response header.
    headers['x-route-trace'].must_equal trace.map{|t| t.join('=')}.join(' | ')

    # The innermost Rack app sees the accumulated SCRIPT_NAME, the final
    # delegated PATH_INFO, and the query string passed through unchanged.
    env['coord.inner'].must_equal ['/legacy/v1', '/items', 'x=1&y=2']

    # After return, SCRIPT_NAME/PATH_INFO are restored to the originals.
    env['coord.after'].must_equal ['', '/legacy/v1/items']
    env['SCRIPT_NAME'].must_equal ''
    env['PATH_INFO'].must_equal '/legacy/v1/items'
  end

  it "restores SCRIPT_NAME/PATH_INFO before the outer error handler when the sub-app raises" do
    (status, headers, body), env = call_env('/legacy/v1/boom', 'x=1&y=2')

    status.must_equal 500
    b = +''; body.each{|s| b << s}
    # The outer error handler observes the restored env coordinates
    # (original SCRIPT_NAME/PATH_INFO).  Its own request object's
    # remaining_path shows where the outer routing layer stopped; the
    # middle layer's consumption happened on the middle app's separate
    # request object.
    b.must_equal 'outer error: inner boom ' \
      'script_name="" path_info="/legacy/v1/boom" remaining_path="/v1/boom"'

    # The sub-app still observed its delegated coordinates before raising.
    env['coord.inner'].must_equal ['/legacy/v1', '/boom', 'x=1&y=2']

    # The trace shows all layers up to the failing delegation.
    env['coord.trace'].last.must_equal ['middle delegates', '/boom']

    # After the exception propagated, env coordinates are restored.
    env['coord.after'].must_equal ['', '/legacy/v1/boom']
    env['SCRIPT_NAME'].must_equal ''
    env['PATH_INFO'].must_equal '/legacy/v1/boom'
  end
end