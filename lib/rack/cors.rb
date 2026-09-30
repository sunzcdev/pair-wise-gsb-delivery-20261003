# frozen_string_literal: true

require 'logger'
require_relative 'cors/resources'
require_relative 'cors/resource'
require_relative 'cors/resource_intersection'
require_relative 'cors/evaluation'
require_relative 'cors/result'
require_relative 'cors/version'

module Rack
  class RuleSet
    MODE_FIRST_MATCH = :first_match
    MODE_INTERSECTION = :intersection

    attr_reader :resources, :mode

    def initialize(mode = MODE_FIRST_MATCH)
      @resources = []
      @mode = mode
    end

    def add_resources(resources)
      @resources << resources
    end

    def intersection?
      mode == MODE_INTERSECTION
    end

    def allow(&block)
      resources = cors::Resources.new
      if block.arity == 1
        block.call(resources)
      else
        resources.instance_eval(&block)
      end
      add_resources(resources)
      resources
    end

    def match(path, env, evaluation)
      if intersection?
        match_intersection(path, env, evaluation)
      else
        match_first(path, env, evaluation)
      end
    end

    def resource_for_path(path_info)
      resources.each do |r|
        found = r.resource_for_path(path_info)
        return found if found
      end
      nil
    end

    private

    def cors
      Cors
    end

    def match_first(path, env, evaluation)
      origin = env[cors::HTTP_ORIGIN]
      resources.each do |r|
        next unless r.allow_origin?(origin, env, evaluation)

        found = r.match_resource(path, env, evaluation)
        return [found, nil, true] if found
      end

      [nil, cors::Result::MISS_NO_PATH, origin_allowed?(origin, env, evaluation)]
    end

    def match_intersection(path, env, evaluation)
      origin = env[cors::HTTP_ORIGIN]
      origin_allowed = false
      matched = []

      resources.each do |r|
        unless r.allow_origin?(origin, env, evaluation)
          return [nil, cors::Result::MISS_NO_ORIGIN, origin_allowed]
        end

        origin_allowed = true
        found = r.match_resource(path, env, evaluation)
        return [nil, cors::Result::MISS_NO_PATH, true] unless found

        matched << found
      end

      if origin_allowed && matched.any?
        return [cors::ResourceIntersection.new(matched), nil, true]
      end

      error = origin_allowed ? cors::Result::MISS_NO_PATH : cors::Result::MISS_NO_ORIGIN
      [nil, error, origin_allowed]
    end

    def origin_allowed?(origin, env, evaluation)
      resources.any? { |r| r.allow_origin?(origin, env, evaluation) }
    end
  end

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

    def initialize(app, opts = {}, &block)
      @app = app
      @debug_mode = !!opts[:debug]
      @default_mode = determine_mode(opts)
      @rule_sets = [RuleSet.new(@default_mode)]
      @default_rule_set = @rule_sets.first
      @current_rule_set = @default_rule_set
      @intersection_depth = 0
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
      @current_rule_set.allow(&block)
    end

    def intersection(&block)
      rule_set = RuleSet.new(RuleSet::MODE_INTERSECTION)
      rule_sets << rule_set

      previous_rule_set = @current_rule_set
      @current_rule_set = rule_set
      @intersection_depth += 1
      if block.arity == 1
        block.call(self)
      else
        instance_eval(&block)
      end
    ensure
      @intersection_depth -= 1
      @current_rule_set = previous_rule_set
      if @intersection_depth.zero? && previous_rule_set.equal?(@default_rule_set)
        @current_rule_set = RuleSet.new(RuleSet::MODE_FIRST_MATCH)
        rule_sets << @current_rule_set
      end
    end
    alias intersect intersection

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
      if vary_resource
        vary = headers['vary']
        cors_vary_headers = if vary_resource.vary_headers&.any?
                              vary_resource.vary_headers
                            else
                              DEFAULT_VARY_HEADERS
                            end
        headers['vary'] = ((vary ? [vary].flatten.map { |v| v.split(/,\s*/) }.flatten : []) + cors_vary_headers).uniq.join(', ')
      end

      result = env[ENV_KEY]
      result.append_header(headers) if debug? && result

      [status, headers, body]
    end

    protected

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

    def rule_sets
      @rule_sets ||= []
    end

    def determine_mode(opts)
      mode = opts[:mode] || (opts[:intersection] ? RuleSet::MODE_INTERSECTION : nil)
      mode.to_s == RuleSet::MODE_INTERSECTION.to_s ? RuleSet::MODE_INTERSECTION : RuleSet::MODE_FIRST_MATCH
    end

    def all_resources
      rule_sets.reject { |rule_set| rule_set.equal?(@default_rule_set) && rule_set.resources.empty? }
               .flat_map(&:resources)
    end

    def process_preflight(env, path)
      result = Result.preflight(env)

      policy, error = match_resource(path, env)
      unless policy
        result.miss(error)
        return {}
      end

      policy.process_preflight(env, result)
    end

    def process_cors(env, path)
      policy, error = match_resource(path, env)
      if policy
        Result.hit(env)
        policy.to_headers(env)
      else
        Result.miss(env, error)
        nil
      end
    end

    def resource_for_path(path_info)
      rule_sets.each do |rule_set|
        found = rule_set.resource_for_path(path_info)
        return found if found
      end
      nil
    end

    def match_resource(path, env)
      evaluation = Evaluation.new
      origin_matched = false
      match_error = Result::MISS_NO_ORIGIN

      rule_sets.each do |rule_set|
        next if rule_set.resources.empty?

        found, error, allowed = rule_set.match(path, env, evaluation)
        return [found, nil] if found

        origin_matched ||= allowed
        match_error = error
      end

      [nil, origin_matched ? Result::MISS_NO_PATH : match_error]
    end
  end
end
