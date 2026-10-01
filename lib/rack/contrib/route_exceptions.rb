# frozen_string_literal: true

module Rack
  class RouteExceptions
    ROUTES = {
      Exception => '/error/internal'
    }

    PATH_INFO = 'rack.route_exceptions.path_info'.freeze
    EXCEPTION = 'rack.route_exceptions.exception'.freeze
    RETURNED  = 'rack.route_exceptions.returned'.freeze

    class << self
      def route(exception, to)
        ROUTES[exception] = to
      end

      alias []= route
    end

    def initialize(app, routes = nil)
      @app = app
      @routes = self.class::ROUTES.dup
      build_routes(routes).each { |exception, to| @routes[exception] = to }
    end

    def call(env, try_again = true)
      returned = @app.call(env)
    rescue Exception => exception
      raise(exception) unless try_again

      to = route_for(exception)
      to ? dispatch(to, env, returned, exception) : raise(exception)
    end

    private

    def route_for(exception)
      ancestors = exception.class.ancestors
      best_klass = nil
      best_index = nil
      @routes.each do |klass, _to|
        index = ancestors.index(klass)
        next if index.nil?
        next if best_index && best_index <= index
        best_klass = klass
        best_index = index
      end
      best_klass && @routes[best_klass]
    end

    def build_routes(routes)
      return [] if routes.nil?
      return routes.to_a if routes.is_a?(Hash)
      routes
    end

    def dispatch(to, env, returned, exception)
      original_path_info = env['PATH_INFO']
      saved = [PATH_INFO, EXCEPTION, RETURNED].map do |key|
        [key, env.key?(key), env[key]]
      end

      env[PATH_INFO] = env['PATH_INFO']
      env[EXCEPTION] = exception
      env[RETURNED]  = returned
      env['PATH_INFO'] = to

      call(env, false)
    ensure
      env['PATH_INFO'] = original_path_info
      saved.each do |key, present, value|
        present ? env[key] = value : env.delete(key)
      end
    end
  end
end
