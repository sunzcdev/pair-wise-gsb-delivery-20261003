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

    # Request headers that actually participate in the CORS decision.
    # They must be listed on every response whose content may depend on
    # them (both successful and rejected) so that caches do not serve a
    # response generated for one kind of request to another.
    DEFAULT_VARY_HEADERS           = ['Origin'].freeze
    PREFLIGHT_EXTRA_VARY_HEADERS   = [
      'Access-Control-Request-Method',
      'Access-Control-Request-Headers'
    ].freeze
    DEFAULT_PREFLIGHT_VARY_HEADERS =
      (DEFAULT_VARY_HEADERS + PREFLIGHT_EXTRA_VARY_HEADERS).freeze
    VARY_HEADER = 'vary'
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
          unless Rack::Utils.valid_path?(path)
            return [400, apply_vary_headers({}, DEFAULT_PREFLIGHT_VARY_HEADERS), []]
          end

          headers = process_preflight(env, path)
          debug(env) do
            "Preflight Headers:\n" +
              headers.collect { |kv| "  #{kv.join(': ')}" }.join("\n")
          end
          headers = apply_vary_headers(headers, vary_headers_for(resource_for_path(path), true))
          result = env[ENV_KEY]
          result.append_header(headers) if debug? && result
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

      status, headers, body = @app.call env

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
      headers = apply_vary_headers(headers, vary_headers_for(vary_resource, false)) if vary_resource

      result = env[ENV_KEY]
      result.append_header(headers) if debug? && result

      [status, headers, body]
    end

    protected

    def vary_headers_for(resource, preflight)
      base = preflight ? DEFAULT_PREFLIGHT_VARY_HEADERS : DEFAULT_VARY_HEADERS
      extra = resource&.vary_headers || []
      (base + extra).uniq
    end

    # Merges the headers participating in the CORS decision into the response
    # Vary header.
    #
    # Existing Vary values are preserved, field names are compared
    # case-insensitively, array valued Vary headers are supported and an
    # existing "*" short-circuits further additions (a Vary of "*" already
    # matches every request). Frozen response headers are duplicated instead
    # of mutated.
    def apply_vary_headers(headers, cors_vary_headers)
      existing_key = headers.keys.detect { |key| key.to_s.downcase == VARY_HEADER }
      existing_vary = headers[existing_key] if existing_key
      downstream = [existing_vary].flatten.compact
                                   .flat_map { |value| value.to_s.split(/,\s*/) }
                                   .reject(&:empty?)

      merged = merge_vary_fields(downstream, [cors_vary_headers].flatten)

      headers = headers.dup
      headers.keys.each { |key| headers.delete(key) if key.to_s.downcase == VARY_HEADER }
      headers[VARY_HEADER] = merged.join(', ')
      headers
    end

    def merge_vary_fields(existing, additions)
      seen = existing.map(&:downcase)
      return [VARY_WILDCARD] if seen.include?(VARY_WILDCARD)

      additions.each_with_object(existing.dup) do |field, result|
        next if field == VARY_WILDCARD

        name = field.to_s
        next if name.empty? || seen.include?(name.downcase)

        seen << name.downcase
        result << name
      end
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
