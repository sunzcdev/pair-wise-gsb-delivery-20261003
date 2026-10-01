# frozen_string_literal: true

module Rack
  class RouteExceptions
    ROUTES = [
      [Exception, '/error/internal']
    ]

    PATH_INFO = 'rack.route_exceptions.path_info'.freeze
    EXCEPTION = 'rack.route_exceptions.exception'.freeze
    RETURNED  = 'rack.route_exceptions.returned'.freeze

    UNSET = Object.new.freeze

    class << self
      def route(exception, to)
        ROUTES.delete_if{|k,v| k == exception }
        ROUTES << [exception, to]
      end

      alias []= route
    end

    # +routes+ maps exception classes to the path their error page is
    # served from. They apply only to this middleware instance and override
    # the class-level routes registered via RouteExceptions.route.
    def initialize(app, routes = {})
      @app = app

      @routes = ROUTES.dup
      routes.each do |exception, to|
        @routes.delete_if { |klass, _path| klass == exception }
        @routes << [exception, to]
      end
    end

    def call(env, try_again = true)
      returned = @app.call(env)
    rescue Exception => exception
      raise(exception) unless try_again

      to = route_for(exception)
      return route(to, env, returned, exception) if to

      raise(exception)
    end

    # Pick the most specific registered exception class matching
    # +exception+, walking its inheritance chain. When the same class is
    # registered more than once, the latest registration wins.
    def route_for(exception)
      exception.class.ancestors.each do |ancestor|
        match = nil
        @routes.each do |klass, to|
          match = to if klass == ancestor
        end
        return match if match
      end

      nil
    end

    def route(to, env, returned, exception)
      previous = {
        PATH_INFO       => original(env, PATH_INFO),
        EXCEPTION       => original(env, EXCEPTION),
        RETURNED        => original(env, RETURNED),
        'PATH_INFO'     => original(env, 'PATH_INFO')
      }

      env.merge!(
        PATH_INFO => env['PATH_INFO'],
        EXCEPTION => exception,
        RETURNED => returned
      )
      env['PATH_INFO'] = to

      call(env, false)
    ensure
      restore(env, previous)
    end

    # Restore the caller's env after the error page has been served, or
    # after it raised itself. Keys that were absent beforehand are removed
    # rather than left holding nil.
    def restore(env, previous)
      previous.each do |key, value|
        if value.equal?(UNSET)
          env.delete(key)
        else
          env[key] = value
        end
      end
    end

    def original(env, key)
      env.key?(key) ? env[key] : UNSET
    end
  end
end
