# frozen_string_literal: true

module Faraday
  class Response
    # RaiseError is a Faraday middleware that raises exceptions on common HTTP
    # client or server error responses.
    class RaiseError < Middleware
      # rubocop:disable Naming/ConstantName
      ClientErrorStatuses = (400...500)
      ServerErrorStatuses = (500...600)
      ClientErrorStatusesWithCustomExceptions = {
        400 => Faraday::BadRequestError,
        401 => Faraday::UnauthorizedError,
        403 => Faraday::ForbiddenError,
        404 => Faraday::ResourceNotFound,
        408 => Faraday::RequestTimeoutError,
        409 => Faraday::ConflictError,
        422 => Faraday::UnprocessableContentError,
        429 => Faraday::TooManyRequestsError
      }.freeze
      # rubocop:enable Naming/ConstantName

      DEFAULT_OPTIONS = {
        include_request: true,
        allowed_statuses: [].freeze,
        response_stages: false
      }.freeze

      def on_request(env)
        # Announce the opt-out on the shared Env so that errors raised by other
        # middlewares (e.g. ParsingError from the Json middleware) honor it too.
        env[:include_request] = false unless options[:include_request]
        # Ask the adapter to leave a snapshot of the response as it was when it
        # finished, before any response middleware transformed it.
        env[:capture_raw_response] = true if options[:response_stages]
      end

      def on_complete(env)
        if Array(options[:allowed_statuses]).include?(env[:status])
          release_raw_response(env)
          return
        end

        case env[:status]
        when *ClientErrorStatusesWithCustomExceptions.keys
          raise ClientErrorStatusesWithCustomExceptions[env[:status]], response_values(env)
        when 407
          # mimic the behavior that we get with proxy requests with HTTPS
          msg = %(407 "Proxy Authentication Required")
          raise Faraday::ProxyAuthError.new(msg, response_values(env))
        when ClientErrorStatuses
          raise Faraday::ClientError, response_values(env)
        when ServerErrorStatuses
          raise Faraday::ServerError, response_values(env)
        when nil
          raise Faraday::NilStatusError, response_values(env)
        else
          release_raw_response(env)
        end
      end

      # Returns a hash of response data with the following keys:
      #   - status
      #   - headers
      #   - body
      #   - request
      #
      # The `request` key is omitted when the middleware is explicitly
      # configured with the option `include_request: false`. When
      # `response_stages: true` is configured, the raw adapter response
      # snapshot is added under a private key consumed by Faraday::Error.
      def response_values(env)
        response = {
          status: env.status,
          headers: env.response_headers,
          body: env.body
        }

        # Include the request data by default. If the middleware was explicitly
        # configured to _not_ include request data, then omit it.
        if options[:include_request]
          response.merge!(
            request: {
              method: env.method,
              url: env.url,
              url_path: env.url.path,
              params: query_params(env),
              headers: env.request_headers,
              body: env.request_body
            }
          )
        end

        raw = env.custom_members[Faraday::RAW_ADAPTER_RESPONSE_KEY]
        response[Faraday::RAW_ADAPTER_RESPONSE_KEY] = raw if raw
        response
      end

      def query_params(env)
        env.request.params_encoder ||= Faraday::Utils.default_params_encoder
        env.params_encoder.decode(env.url.query)
      end

      # Removes the private raw-response marker from successful responses so it
      # never leaks on the Env of a response that did not raise an error.
      def release_raw_response(env)
        env.custom_members.delete(Faraday::RAW_ADAPTER_RESPONSE_KEY)
        env.custom_members.delete(:capture_raw_response)
      end
    end
  end
end

Faraday::Response.register_middleware(raise_error: Faraday::Response::RaiseError)
