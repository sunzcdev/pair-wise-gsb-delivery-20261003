# frozen_string_literal: true

require_relative 'constants'
require_relative 'lint'

module Rack
  # Rack::Diagnostics provides an opt-in, layered contract checking mode for
  # applications assembled with Rack::Builder.
  #
  # When diagnostics are enabled, every component in the middleware stack is
  # wrapped in a Rack::Diagnostics::Boundary. Each boundary uses a regular
  # Rack::Lint to validate the Rack contract on both sides of the component,
  # and records a redacted summary of what crossed the boundary. A contract
  # violation raises a Rack::Diagnostics::ContractViolation that names the
  # component whose boundary detected the problem and includes the summary of
  # the previous boundary that validated successfully.
  #
  # The diagnostics are passive and lazy:
  #
  # * The wrapped application is invoked exactly once, never re-called.
  # * Streaming/enumerable bodies are never iterated eagerly; violations that
  #   surface while a body is iterated later are still attributed to the
  #   component whose boundary created the body wrapper.
  # * Reports only contain structural metadata (status, header names, body
  #   class, sizes). Header values and body contents are never written into a
  #   report, so secrets and payloads stay out of the diagnostic output.
  #
  # When diagnostics are not enabled, Rack::Builder behaves exactly as before
  # and none of this code is loaded.
  module Diagnostics
    # Raised when a component boundary detects a Rack contract violation.
    # It is a Rack::Lint::LintError so existing Lint-based error handling
    # keeps working.
    class ContractViolation < Lint::LintError
      # @return [String] human readable name of the component whose boundary
      #   detected the violation.
      attr_reader :component

      # @return [:request, :response, :body] when the violation was detected:
      #   while validating the incoming environment, the returned response
      #   tuple, or while lazily iterating/closing the response body.
      attr_reader :phase

      # @return [Rack::Lint::LintError] the original lint failure.
      attr_reader :cause_error

      # @return [Diagnostics::Summary, nil] redacted summary captured at the
      #   previous boundary that validated successfully.
      attr_reader :previous_summary

      def initialize(component, phase, cause_error, previous_summary)
        @component = component
        @phase = phase
        @cause_error = cause_error
        @previous_summary = previous_summary

        super(build_message)
        set_backtrace(cause_error.backtrace)
      end

      private

      def build_message
        lines = []
        lines << "Rack contract violation at #{@component} (#{@phase} phase)"
        lines << "Lint error: #{@cause_error.message}"
        if @previous_summary
          lines << "Previous boundary summary (#{@previous_summary.kind}):"
          lines << @previous_summary.describe
        else
          lines << "Previous boundary summary: none"
        end
        lines.join("\n")
      end
    end

    # Redacted, value-free description of the data observed at a boundary.
    # Subclasses describe either the incoming request or the outgoing
    # response. Header values and body contents are intentionally absent.
    class Summary
      # @return [String] name of the component that recorded this summary.
      attr_reader :component

      def initialize(component)
        @component = component
      end

      def kind
        self.class.name.split('::').last
      end

      # Render the summary as indented, redacted text.
      def describe
        details.map { |key, value| "  #{key}: #{value}" }.join("\n")
      end

      private

      def details
        raise NotImplementedError
      end

      # Only ever render header *names*, never values, and cap the output so
      # a report cannot grow without bound.
      def header_names(headers)
        names = headers.keys
        shown = names.first(MAX_HEADER_NAMES).map(&:inspect)
        shown << "... (#{names.size - MAX_HEADER_NAMES} more)" if names.size > MAX_HEADER_NAMES
        shown.join(', ')
      end

      def truncate(value, limit = MAX_TEXT)
        text = value.to_s
        text = text.byteslice(0, limit) if text.bytesize > limit
        text
      end
    end

    class RequestSummary < Summary
      def initialize(component, env)
        super(component)

        @method = env[REQUEST_METHOD]
        @path = env[PATH_INFO]
        @env_key_count = env.size
        @http_header_names = env.keys.grep(/\AHTTP_/)
      end

      private

      def details
        {
          "component" => @component,
          "request method" => truncate(@method),
          "path (no query string)" => truncate(@path),
          "environment keys" => @env_key_count,
          "request header names" => @http_header_names.first(MAX_HEADER_NAMES).map(&:inspect).join(', ')
        }
      end
    end

    class ResponseSummary < Summary
      # @return [Integer, nil] response status code observed at the boundary.
      attr_accessor :status

      # @return [Hash] the response headers; only their names are reported.
      attr_accessor :headers

      # @return [Object] the original (un-wrapped) response body object.
      attr_accessor :body

      # @return [Integer] number of body chunks observed while iterating.
      attr_accessor :chunks

      # @return [Integer] number of body bytes observed while iterating.
      attr_accessor :bytes

      def initialize(component)
        super
        @chunks = 0
        @bytes = 0
      end

      # Whether the body is a streaming body (responds to #call and not
      # #each). Streaming bodies are not iterated by the diagnostics, so
      # chunk and byte counts stay at zero.
      def streaming?
        body.respond_to?(:call) && !body.respond_to?(:each)
      end

      private

      def details
        {
          "component" => @component,
          "status" => @status.inspect,
          "header count" => @headers.size,
          "response header names" => header_names(@headers),
          "body class" => body.class.name || body.class.inspect,
          "body object id" => body.object_id.to_s,
          "body responds to" => BODY_METHODS.keys.select { |name| body.respond_to?(name) }.join(', '),
          "streaming body" => streaming?.inspect,
          "iterated chunks" => @chunks,
          "iterated bytes" => @bytes
        }
      end
    end

    # Per-request bookkeeping shared by all boundaries in a stack. The
    # environment itself is the only object that travels through every
    # component, so the trail is stored in reserved +rack.diagnostics+ keys.
    class Frame
      # @return [String] name of the component owning this frame.
      attr_reader :component

      # @return [RequestSummary] summary of the environment entering the
      #   component.
      attr_reader :request_summary

      # @return [ResponseSummary, nil] summary of the response the component
      #   returned, once it has validated successfully.
      attr_accessor :response_summary

      def initialize(component, request_summary)
        @component = component
        @request_summary = request_summary
        @response_summary = nil
      end
    end

    # A single component boundary. The wrapped component is invoked exactly
    # once; a Rack::Lint performs the actual contract checks while this
    # object owns component attribution, boundary summaries and the lazy body
    # wrapper.
    class Boundary
      # @param app [#call] the component (middleware instance or endpoint)
      #   whose boundary this represents.
      # @param label [String] human readable name used in reports.
      def initialize(app, label = Diagnostics.label_for(app))
        @app = app
        @label = label
      end

      attr_reader :label

      def call(env)
        # Per-request state is kept in local objects rather than in instance
        # variables, so a boundary works correctly even when frozen by
        # Rack::Builder#freeze_app.
        tracer = Tracer.new(@app)
        lint = Lint.new(tracer)
        frame = open_frame(env)

        begin
          status, headers, lint_body = lint.call(env)

          summary = ResponseSummary.new(@label)
          summary.status = status
          summary.headers = headers
          summary.body = unwrap_body(lint_body)
          frame.response_summary = summary if frame
          env[LAST_RESPONSE_KEY] = summary if env.is_a?(Hash)

          [status, headers, BodyBoundary.new(lint_body, self, frame, summary)]
        rescue Lint::LintError => error
          # ContractViolation is itself a LintError; never wrap one twice so
          # that the component closest to the failure keeps attribution.
          raise if error.is_a?(ContractViolation)

          # The tracer flips this when the wrapped component is actually
          # invoked. A LintError raised before that point is an invalid
          # incoming environment; one raised after it is an invalid response
          # (whether the component returned or raised).
          phase = tracer.entered? ? :response : :request
          raise ContractViolation.new(@label, phase, error, previous_summary(env, frame, phase))
        ensure
          close_frame(env)
        end
      end

      # Convert a lint failure that surfaced while lazily consuming the body
      # into a ContractViolation tied to the component that created the body
      # wrapper, not whatever happens to be iterating it later.
      def body_contract_violation(error, frame, summary)
        previous = frame&.response_summary || summary
        ContractViolation.new(@label, :body, error, previous)
      end

      private

      # The tracer marks the moment the wrapped component is actually
      # entered, which lets the surrounding rescue distinguish an invalid
      # incoming environment from an invalid returned response.
      class Tracer
        def initialize(app)
          @app = app
          @entered = false
        end

        def entered?
          @entered
        end

        def call(env)
          @entered = true
          @app.call(env)
        end
      end

      def open_frame(env)
        # A non-Hash environment is itself a contract violation; leave it to
        # Rack::Lint to report and skip boundary bookkeeping rather than
        # calling Hash-only methods on it.
        return nil unless env.is_a?(Hash)

        env[FRAME_STACK_KEY] ||= []
        frame = Frame.new(@label, RequestSummary.new(@label, env))
        env[FRAME_STACK_KEY] << frame
        frame
      end

      def close_frame(env)
        return unless env.is_a?(Hash)

        stack = env[FRAME_STACK_KEY]
        stack.pop if stack
      end

      def previous_summary(env, frame, phase)
        return nil unless env.is_a?(Hash) && frame

        case phase
        when :response
          # The most recently validated response is the previous boundary:
          # either the one recorded by an inner component, or this boundary's
          # own request summary if the component is the innermost one.
          env[LAST_RESPONSE_KEY] || frame.request_summary
        when :request
          # An invalid environment comes from the caller, whose frame is the
          # one directly below this boundary in the stack.
          stack = env[FRAME_STACK_KEY]
          index = stack.index(frame)
          if index && index > 0
            stack[index - 1].request_summary
          end
        end
      end

      # Reach through Rack::Lint wrappers and the diagnostics body wrappers
      # of inner boundaries to identify the component's real body, so the
      # summary reports the component's body class rather than a wrapper.
      def unwrap_body(body)
        while body.instance_variable_defined?(:@body)
          body = body.instance_variable_get(:@body)
        end
        body
      end
    end

    # Wraps the lint-validated body returned by a boundary. Every method is
    # forwarded without iterating eagerly, and a lint failure raised during a
    # later iteration is re-raised as a ContractViolation attributed to the
    # boundary's component.
    class BodyBoundary
      def initialize(body, boundary, frame, response_summary)
        @body = body
        @boundary = boundary
        @frame = frame
        @response_summary = response_summary
      end

      def each(&block)
        @body.each do |chunk|
          @response_summary.chunks += 1
          @response_summary.bytes += chunk.bytesize
          yield chunk
        end
      rescue Lint::LintError => error
        raise if error.is_a?(ContractViolation)
        raise @boundary.body_contract_violation(error, @frame, @response_summary)
      end

      def call(stream)
        @body.call(stream)
      rescue Lint::LintError => error
        raise if error.is_a?(ContractViolation)
        raise @boundary.body_contract_violation(error, @frame, @response_summary)
      end

      def close
        @body.close if @body.respond_to?(:close)
      rescue Lint::LintError => error
        raise if error.is_a?(ContractViolation)
        raise @boundary.body_contract_violation(error, @frame, @response_summary)
      end

      def to_path
        @body.to_path
      end

      def to_ary
        @body.to_ary
      rescue Lint::LintError => error
        raise if error.is_a?(ContractViolation)
        raise @boundary.body_contract_violation(error, @frame, @response_summary)
      end

      def respond_to?(name, *)
        if BODY_METHODS.key?(name)
          @body.respond_to?(name)
        else
          super
        end
      end
    end

    # Build a stable, human readable name for a component. Middleware and
    # application classes use their class name; anonymous objects, procs and
    # methods fall back to a short structural description plus source
    # location when one is available.
    def self.label_for(component)
      klass = component.class
      return "#{klass.name} instance" if klass.name && !klass.name.empty?

      case component
      when Proc
        location = component.source_location
        location ? "Proc (#{location.join(':')})" : "Proc"
      else
        if component.respond_to?(:source_location)
          location = component.source_location
          location ? "Method (#{location.join(':')})" : "Method"
        else
          "#{klass.inspect} instance"
        end
      end
    end

    MAX_HEADER_NAMES = 20
    private_constant :MAX_HEADER_NAMES

    MAX_TEXT = 200
    private_constant :MAX_TEXT

    BODY_METHODS = {each: true, call: true, to_path: true, to_ary: true, close: true}.freeze
    private_constant :BODY_METHODS

    FRAME_STACK_KEY = 'rack.diagnostics.frames'
    LAST_RESPONSE_KEY = 'rack.diagnostics.last_response'
    private_constant :FRAME_STACK_KEY, :LAST_RESPONSE_KEY
  end
end
