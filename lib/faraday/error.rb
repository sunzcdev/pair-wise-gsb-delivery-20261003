# frozen_string_literal: true

# Faraday namespace.
module Faraday
  # Key used by the RaiseError middleware to carry the raw adapter response
  # while two-stage response views are enabled. Defined here so that
  # Faraday::Error can read it without loading response middleware.
  RAW_ADAPTER_RESPONSE_KEY = :raw_adapter_response

  # Faraday error base class.
  class Error < StandardError
    attr_reader :response, :wrapped_exception, :response_stages

    def initialize(exc = nil, response = nil)
      @wrapped_exception = nil unless defined?(@wrapped_exception)
      @response = nil unless defined?(@response)
      @response_stages = nil unless defined?(@response_stages)
      super(exc_msg_and_response!(exc, response))
    end

    def backtrace
      if @wrapped_exception
        @wrapped_exception.backtrace
      else
        super
      end
    end

    def inspect
      inner = +''
      inner << " wrapped=#{@wrapped_exception.inspect}" if @wrapped_exception
      inner << " response=#{@response.inspect}" if @response
      inner << " #{super}" if inner.empty?
      %(#<#{self.class}#{inner}>)
    end

    def response_status
      return unless @response

      @response.is_a?(Faraday::Response) ? @response.status : @response[:status]
    end

    def response_headers
      return unless @response

      @response.is_a?(Faraday::Response) ? @response.headers : @response[:headers]
    end

    def response_body
      return unless @response

      @response.is_a?(Faraday::Response) ? @response.body : @response[:body]
    end

    # Two-stage view of the response, available when the RaiseError middleware
    # is configured with `response_stages: true`.
    #
    # @return [Faraday::ResponseStages, nil] the response as seen by the adapter
    #   and the response at the point where the error was raised, or nil when
    #   the feature was not enabled.
    def raw_response
      @response_stages&.raw
    end

    # The response captured when the adapter finished, before any response
    # middleware transformed it.
    #
    # @return [Hash, Faraday::Response, nil] a snapshot of the adapter response.
    def adapter_response
      @response_stages&.adapter
    end

    # The response at the point where the error was raised, after any response
    # middleware positioned before RaiseError had run. This is the same object
    # returned by {#response} when two stages were captured.
    #
    # @return [Hash, Faraday::Response, nil] a snapshot of the current response.
    def current_response
      @response_stages&.current
    end

    protected

    # Pulls out potential parent exception and response hash, storing them in
    # instance variables.
    # exc      - Either an Exception, a string message, or a response hash.
    # response - Hash
    #              :status  - Optional integer HTTP response status
    #              :headers - String key/value hash of HTTP response header
    #                         values.
    #              :body    - Optional string HTTP response body.
    #              :request - Hash
    #                           :method   - Symbol with the request HTTP method.
    #                           :url      - URI object with the url requested.
    #                           :url_path - String with the url path requested.
    #                           :params   - String key/value hash of query params
    #                                     present in the request.
    #                           :headers  - String key/value hash of HTTP request
    #                                     header values.
    #                           :body     - String HTTP request body.
    #
    # If a subclass has to call this, then it should pass a string message
    # to `super`. See NilStatusError.
    def exc_msg_and_response!(exc, response = nil)
      if @response.nil? && @wrapped_exception.nil?
        exc, response = extract_response_stages!(exc, response)
        @wrapped_exception, msg, @response = exc_msg_and_response(exc, omit_request_data(response))
        return msg
      end

      exc.to_s
    end

    # Pulls out potential parent exception and response hash.
    def exc_msg_and_response(exc, response = nil)
      case exc
      when Exception
        [exc, exc.message, response]
      when Hash
        [nil, build_error_message_from_hash(exc), exc]
      when Faraday::Env
        sanitized = omit_request_data(exc)
        message =
          if sanitized.equal?(exc)
            build_error_message_from_env(exc)
          else
            fallback_error_message(exc.status)
          end
        [nil, message, sanitized]
      else
        [nil, exc.to_s, response]
      end
    end

    private

    # Detects the optional two-stage view attached by RaiseError. The raw
    # adapter snapshot is carried on the Env (or inside the response hash) and
    # is removed from the stored response so `#response` keeps its historical
    # shape. Returns the (possibly updated) exception and response arguments.
    def extract_response_stages!(exc, response)
      if exc.is_a?(Hash) && exc.key?(Faraday::RAW_ADAPTER_RESPONSE_KEY)
        raw = exc.delete(Faraday::RAW_ADAPTER_RESPONSE_KEY)
        @response_stages = Faraday::ResponseStages.new(raw: raw, current: exc)
      elsif response.is_a?(Hash) && response.key?(Faraday::RAW_ADAPTER_RESPONSE_KEY)
        raw = response.delete(Faraday::RAW_ADAPTER_RESPONSE_KEY)
        @response_stages = Faraday::ResponseStages.new(raw: raw, current: response)
      elsif response.is_a?(Faraday::Response)
        extract_stages_from_env!(response.env, response)
      end

      extract_stages_from_env!(exc, exc) if exc.is_a?(Faraday::Env)
      [exc, response]
    end

    def extract_stages_from_env!(env, current)
      return unless env.is_a?(Faraday::Env)

      raw = env.custom_members.delete(Faraday::RAW_ADAPTER_RESPONSE_KEY)
      return unless raw

      @response_stages ||= Faraday::ResponseStages.new(raw: raw, current: current)
    end

    # When the RaiseError middleware opted this request out of request data
    # (`include_request: false`), it marks the Env. Build a fresh Response from
    # response fields alone to preserve APIs such as `on_complete` without
    # retaining request fields or custom members from the original Env.
    # The body reads the explicit `response_body` member because `Env#body`
    # aliases to the request body while the status is nil.
    def omit_request_data(response)
      env =
        case response
        when Faraday::Response
          response.env if response.finished?
        when Faraday::Env
          response
        end
      return response unless env.is_a?(Faraday::Env) && env[:include_request] == false

      Faraday::Response.new(
        status: env.status,
        response_headers: env.response_headers,
        response_body: env.response_body
      )
    end

    def build_error_message_from_hash(hash)
      # Be defensive with external Hash objects - they might be missing keys
      status = hash.fetch(:status, nil)
      request = hash.fetch(:request, nil)

      return fallback_error_message(status) if request.nil?

      method = request.fetch(:method, nil)
      url = request.fetch(:url, nil)
      build_status_error_message(status, method, url)
    end

    def build_error_message_from_env(env)
      # Faraday::Env is internal - we can make reasonable assumptions about its structure
      build_status_error_message(env.status, env.method, env.url)
    end

    def build_status_error_message(status, method, url)
      method_str = method ? method.to_s.upcase : ''
      url_str = url ? url.to_s : ''
      "the server responded with status #{status} for #{method_str} #{url_str}"
    end

    def fallback_error_message(status)
      "the server responded with status #{status} - method and url are not available " \
        'due to include_request: false on Faraday::Response::RaiseError middleware'
    end
  end

  # Faraday client error class. Represents 4xx status responses.
  class ClientError < Error
  end

  # Raised by Faraday::Response::RaiseError in case of a 400 response.
  class BadRequestError < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 401 response.
  class UnauthorizedError < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 403 response.
  class ForbiddenError < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 404 response.
  class ResourceNotFound < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 407 response.
  class ProxyAuthError < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 408 response.
  class RequestTimeoutError < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 409 response.
  class ConflictError < ClientError
  end

  # Raised by Faraday::Response::RaiseError in case of a 422 response.
  class UnprocessableContentError < ClientError
  end

  # Used to provide compatibility with legacy error name.
  UnprocessableEntityError = UnprocessableContentError

  # Raised by Faraday::Response::RaiseError in case of a 429 response.
  class TooManyRequestsError < ClientError
  end

  # Faraday server error class. Represents 5xx status responses.
  class ServerError < Error
  end

  # A unified client error for timeouts.
  class TimeoutError < ServerError
    def initialize(exc = 'timeout', response = nil)
      super
    end
  end

  # Raised by Faraday::Response::RaiseError in case of a nil status in response.
  class NilStatusError < ServerError
    def initialize(exc, response = nil)
      exc_msg_and_response!(exc, response)
      super('http status could not be derived from the server response')
    end
  end

  # A unified error for failed connections.
  class ConnectionFailed < Error
  end

  # A unified client error for SSL errors.
  class SSLError < Error
  end

  # Raised by middlewares that parse the response, like the JSON response middleware.
  class ParsingError < Error
  end

  # Raised by Faraday::Middleware and subclasses when invalid default_options are used
  class InitializationError < Error
  end
end
