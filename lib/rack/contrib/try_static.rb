# frozen_string_literal: true

module Rack

  # The Rack::TryStatic middleware delegates requests to Rack::Static middleware
  # trying to match a static file
  #
  # Examples
  #
  # use Rack::TryStatic,
  #   :root => "public",  # static files root dir
  #   :urls => %w[/],     # match all requests
  #   :try => ['.html', 'index.html', '/index.html'] # try these postfixes sequentially
  #
  #   uses same options as Rack::Static with extra :try option which is an array
  #   of postfixes to find desired file

  class TryStatic

    def initialize(app, options)
      @app = app
      @try = ['', *options[:try]]
      @static = ::Rack::Static.new(
        lambda { |_| [404, {}, []] },
        options)
    end

    def call(env)
      orig_path = env['PATH_INFO']
      found = nil
      begin
        @try.each do |path|
          env['PATH_INFO'] = orig_path + path
          resp = @static.call(env)
          status = resp[0]
          if (403..405).include?(status)
            # A response that is only tried for a candidate path is not
            # returned to anyone, so its body has to be closed here.
            resp[2].close if resp[2].respond_to?(:close)
          else
            found = resp
            break
          end
        end
      ensure
        # Keep the temporary candidate path local to this probe so that
        # both the downstream app and the caller see the original path.
        env['PATH_INFO'] = orig_path
      end
      found || @app.call(env)
    end
  end
end
