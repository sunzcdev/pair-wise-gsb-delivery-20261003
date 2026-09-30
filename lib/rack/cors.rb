# frozen_string_literal: true

require 'logger'
require_relative 'cors/resources'
require_relative 'cors/resource'
require_relative 'cors/result'
require_relative 'cors/version'

module Rack
  class Cors
    HTTP_ORIGIN   = 'HTTP_ORIGIN'
    HTTP_X_ORIGIN = 'HTTP_X_ORIGIN'

    HTTP_ACCESS_CONTROL_REQUEST_METHOD  = 'HTTP_ACCESS_CONTROL_REQUEST_METHOD'
    HTTP_ACCESS_CONTROL_REQUEST_HEADERS = 'HTTP_ACCESS_CONTROL_REQUEST_HEADERS'

    PATH_INFO      = 'PATH_INFO'
    REQUEST_METHOD = 'REQUEST_METHOD'

    RACK_LOGGER = 'rack.logger'
    RACK_CORS   =
      # retaining the old key for backwards compatibility
      ENV_KEY = 'rack.cors'

    OPTIONS     = 'OPTIONS'

    DEFAULT_VARY_HEADERS = ['Origin'].freeze
    PREFLIGHT_VARY_HEADERS = [
      'Origin',
      'Access-Control-Request-Method',
      'Access-Control-Request-Headers'
    ].freeze

    VARY_WILDCARD = '*'

    def initialize(app, opts = {}, &block)
      @app = app
      @debug_mode = !!opts[:debug]
      @logger = @logger_proc = nil

      logger = opts[:logger]
      if logger
        if logger.respond_to? :call
          @logger_proc = opts[:logger]
        else
          @logger = logger
        end
      end

      return unless block_given?

      if block.arity == 1
        block.call(self)
      else
        instance_eval(&block)
      end
    end

    def debug?
      @debug_mode
    end

    def allow(&block)
      all_resources << (resources = Resources.new)

      if block.arity == 1
        block.call(resources)
      else
        resources.instance_eval(&block)
      end
    end

    def call(env)
      env[HTTP_ORIGIN] ||= env[HTTP_X_ORIGIN] if env[HTTP_X_ORIGIN]

      path = evaluate_path(env)

      add_headers = nil
      if env[HTTP_ORIGIN]
        debug(env) do
          ['Incoming Headers:',
           "  Origin: #{env[HTTP_ORIGIN]}",
           "  Path-Info: #{path}",
           "  Access-Control-Request-Method: #{env[HTTP_ACCESS_CONTROL_REQUEST_METHOD]}",
           "  Access-Control-Request-Headers: #{env[HTTP_ACCESS_CONTROL_REQUEST_HEADERS]}"].join("\n")
        end

        if env[REQUEST_METHOD] == OPTIONS && env[HTTP_ACCESS_CONTROL_REQUEST_METHOD]
          return [400, {}, []] unless Rack::Utils.valid_path?(path)

          headers = process_preflight(env, path)
          debug(env) do
            "Preflight Headers:\n" +
              headers.collect { |kv| "  #{kv.join(': ')}" }.join("\n")
          end
          headers = apply_vary_headers(headers, cors_vary_headers(resource_for_path(path), true))
          return [200, headers, []]
        else
          add_headers = process_cors(env, path)
        end
      else
        Result.miss(env, Result::MISS_NO_ORIGIN)
      end

      # This call must be done BEFORE calling the app because for some reason
      # env[PATH_INFO] gets changed after that and it won't match. (At least
      # in rails 4.1.6)
      vary_resource = resource_for_path(path)

      status, downstream_headers, body = @app.call env
      headers = downstream_headers.dup

      if add_headers
        headers = add_headers.merge(headers)
        debug(env) do
          add_headers.each_pair do |key, value|
            headers["x-rack-cors-original-#{key}"] = value if headers.key?(key)
          end
        end
      end

      # Vary header should ALWAYS mention Origin if there's ANY chance for the
      # response to be different depending on the Origin header value.
      # Better explained here: http://www.fastly.com/blog/best-practices-for-using-the-vary-header/
      if vary_resource
        headers = apply_vary_headers(headers, cors_vary_headers(vary_resource, false))
      end

      result = env[ENV_KEY]
      result.append_header(headers) if debug? && result

      [status, headers, body]
    end

    protected

    # Merge the CORS decision dependencies into the response's Vary header so
    # caches key on every request header that can influence the decision.
    #
    # * a downstream "*" is authoritative and is left untouched
    # * field names are compared case insensitively
    # * downstream Vary may be a String or an Array of strings
    # * headers are duplicated first so frozen downstream headers are safe
    def apply_vary_headers(headers, cors_vary_headers)
      headers = headers.dup

      downstream = headers['vary']
      downstream_fields = [downstream].flatten.compact.flat_map do |value|
        value.to_s.split(/,\s*/)
      end

      return headers if downstream_fields.map(&:strip).include?(VARY_WILDCARD)

      vary_fields = downstream_fields + cors_vary_headers
      vary_fields = vary_fields.uniq { |field| field.to_s.strip.downcase }

      headers['vary'] = vary_fields.join(', ')
      headers
    end

    # Request headers that actually participate in the CORS decision for the
    # matched resource. Preflight decisions additionally depend on the
    # requested method and requested headers.
    def cors_vary_headers(resource, preflight)
      base = preflight ? PREFLIGHT_VARY_HEADERS : DEFAULT_VARY_HEADERS
      custom = resource&.vary_headers || []
      (base + custom).uniq { |field| field.to_s.strip.downcase }
    end

    def debug(env, message = nil, &block)
      (@logger || select_logger(env)).debug(message, &block) if debug?
    end

    def select_logger(env)
      @logger = if @logger_proc
                  logger_proc = @logger_proc
                  @logger_proc = nil
                  logger_proc.call

                elsif defined?(Rails) && Rails.respond_to?(:logger) && Rails.logger
                  Rails.logger

                elsif env[RACK_LOGGER]
                  env[RACK_LOGGER]

                else
                  ::Logger.new(STDOUT).tap { |logger| logger.level = ::Logger::Severity::DEBUG }
                end
    end

    def evaluate_path(env)
      path = env[PATH_INFO]

      if path
        path = Rack::Utils.unescape_path(path)

        path = Rack::Utils.clean_path_info(path) if Rack::Utils.valid_path?(path)
      end

      path
    end

    def all_resources
      @all_resources ||= []
    end

    def process_preflight(env, path)
      result = Result.preflight(env)

      resource, error = match_resource(path, env)
      unless resource
        result.miss(error)
        return {}
      end

      resource.process_preflight(env, result)
    end

    def process_cors(env, path)
      resource, error = match_resource(path, env)
      if resource
        Result.hit(env)
        cors = resource.to_headers(env)
        cors

      else
        Result.miss(env, error)
        nil
      end
    end

    def resource_for_path(path_info)
      all_resources.each do |r|
        found = r.resource_for_path(path_info)
        return found if found
      end
      nil
    end

    def match_resource(path, env)
      origin = env[HTTP_ORIGIN]

      origin_matched = false
      all_resources.each do |r|
        next unless r.allow_origin?(origin, env)

        origin_matched = true
        found = r.match_resource(path, env)
        return [found, nil] if found
      end

      [nil, origin_matched ? Result::MISS_NO_PATH : Result::MISS_NO_ORIGIN]
    end
  end
end
