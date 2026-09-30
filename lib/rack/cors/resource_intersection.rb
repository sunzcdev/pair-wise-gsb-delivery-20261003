# frozen_string_literal: true

module Rack
  class Cors
    class ResourceIntersection
      attr_reader :resources

      def initialize(resources)
        @resources = resources
      end

      def process_preflight(env, result)
        headers = {}

        request_method = env[Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_METHOD]
        if request_method.nil?
          result.miss(Result::MISS_NO_METHOD)
          return headers
        end

        unless resource_allows_method?(request_method)
          result.miss(Result::MISS_DENY_METHOD)
          return headers
        end

        requested_headers = parse_request_headers(env)
        unless requested_headers.all? { |header| resource_allows_header?(header) }
          result.miss(Result::MISS_DENY_HEADER)
          return headers
        end

        result.hit = true
        headers = header_proc.call(to_headers(env))

        request_header_value = env[Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_HEADERS]
        headers['access-control-allow-headers'] = request_header_value if request_header_value
        headers
      end

      def to_headers(env)
        headers = {
          'access-control-allow-origin' => allow_origin_header(env),
          'access-control-allow-methods' => combined_methods.join(', '),
          'access-control-expose-headers' => combined_expose_headers.join(', '),
          'access-control-max-age' => combined_max_age.to_s
        }
        headers['access-control-allow-credentials'] = 'true' if credentials?
        headers
      end

      def vary_headers
        resources.flat_map { |resource| resource.vary_headers || [] }.uniq
      end

      private

      def resource_allows_method?(method)
        resources.all? { |resource| resource.allows_method?(method) }
      end

      def combined_header_names
        @combined_header_names ||=
          if resources.all? { |resource| resource.headers == :any }
            :any
          else
            resources
              .map { |resource| resource.headers || [] }
              .reject { |headers| headers == :any }
              .reduce(:&)
              .sort
          end
      end

      def resource_allows_header?(header)
        return true if combined_header_names == :any

        normalized_header = header.downcase
        Resource::CORS_SIMPLE_HEADERS.include?(normalized_header) ||
          combined_header_names.include?(normalized_header)
      end

      def parse_request_headers(env)
        value = env[Rack::Cors::HTTP_ACCESS_CONTROL_REQUEST_HEADERS]
        return [] unless value

        value.split(/\s*,\s*/).map(&:strip).reject(&:empty?)
      end

      def allow_origin_header(env)
        return '*' if public_resources?

        env[Rack::Cors::HTTP_ORIGIN]
      end

      def public_resources?
        resources.all?(&:public_resource?)
      end

      def credentials?
        resources.all?(&:credentials)
      end

      def combined_methods
        resources
          .map { |resource| resource.methods.map(&:downcase) }
          .reduce(:&)
          .sort
          .map(&:upcase)
      end

      def combined_expose_headers
        resources
          .map { |resource| (resource.expose || []).map { |header| header.downcase.strip } }
          .reduce(:&)
          .sort
      end

      def combined_max_age
        resources.map(&:max_age).min
      end

      def header_proc
        @header_proc ||=
          if defined?(Rack::Headers)
            ->(headers) { headers }
          else
            ->(headers) { Rack::Utils::HeaderHash.new(headers) }
          end
      end
    end
  end
end
