# frozen_string_literal: true

module Rack
  class Cors
    class CombinedResource
      attr_reader :resources

      def initialize(resources)
        @resources = resources
      end

      def match?(path, env, conditions = Rack::Cors::Conditions.new)
        resources.all? { |resource| resource.match?(path, env, conditions) }
      end

      def process_preflight(env, result)
        headers = {}
        request_method = env[Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_METHOD]
        if request_method.nil?
          result.miss(Result::MISS_NO_METHOD)
          return headers
        end

        unless resources.all? { |resource| resource.methods.include?(request_method.downcase) }
          result.miss(Result::MISS_DENY_METHOD)
          return headers
        end

        request_headers = env[Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_HEADERS]
        if request_headers && resources.any? { |resource| !resource.allow_headers?(request_headers) }
          result.miss(Result::MISS_DENY_HEADER)
          return headers
        end

        result.hit = true
        headers = to_headers(env)
        if request_headers
          headers['access-control-allow-headers'] =
            request_headers.is_a?(String) ? request_headers : request_headers.join(', ')
        end
        headers
      end

      def to_headers(env)
        headers = {
          'access-control-allow-origin' => allow_origin(env[Rack::Cors::HTTP_ORIGIN]),
          'access-control-allow-methods' => methods.join(', '),
          'access-control-max-age' => max_age.to_s
        }
        headers['access-control-allow-credentials'] = 'true' if credentials
        common_expose = expose
        headers['access-control-expose-headers'] = common_expose.join(', ') unless common_expose.empty?
        if defined?(Rack::Headers)
          headers
        else
          Rack::Utils::HeaderHash.new(headers)
        end
      end

      def vary_headers
        values = resources.flat_map { |resource| resource.vary_headers || [] }
        return nil if values.empty?

        values.group_by(&:downcase)
              .sort_by(&:first)
              .map { |_name, headers| headers.min }
      end

      protected

      def allow_origin(origin)
        return '*' if resources.all?(&:public_resource)

        origin
      end

      def credentials
        resources.all?(&:credentials) && !resources.any?(&:public_resource)
      end

      def methods
        resources.map(&:methods).reduce do |common, candidate|
          common & candidate
        end.map(&:upcase).sort
      end

      def expose
        resource_headers = resources.map { |resource| resource.expose || [] }
        common_names = resource_headers.map { |headers| headers.map(&:downcase).uniq }.reduce(:&).sort

        common_names.map do |name|
          resource_headers.flatten.select { |header| header.casecmp?(name) }.min
        end
      end

      def max_age
        resources.map(&:max_age).min
      end
    end
  end
end
