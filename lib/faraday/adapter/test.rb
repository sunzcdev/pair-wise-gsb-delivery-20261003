# frozen_string_literal: true

require 'timeout'

module Faraday
  class Adapter
    # @example
    #   test = Faraday::Connection.new do
    #     use Faraday::Adapter::Test do |stub|
    #       # Define matcher to match the request
    #       stub.get '/resource.json' do
    #         # return static content
    #         [200, {'Content-Type' => 'application/json'}, 'hi world']
    #       end
    #
    #       # response with content generated based on request
    #       stub.get '/showget' do |env|
    #         [200, {'Content-Type' => 'text/plain'}, env[:method].to_s]
    #       end
    #
    #       # A regular expression can be used as matching filter
    #       stub.get /\A\/items\/(\d+)\z/ do |env, meta|
    #         # in case regular expression is used, an instance of MatchData
    #         # can be received
    #         [200,
    #          {'Content-Type' => 'text/plain'},
    #          "showing item: #{meta[:match_data][1]}"
    #         ]
    #       end
    #
    #      # Test the request body is the same as the stubbed body
    #      stub.post('/bar', 'name=YK&word=call') { [200, {}, ''] }
    #
    #      # You can pass a proc as a stubbed body and check the request body in your way.
    #      # In this case, the proc should return true or false.
    #      stub.post('/foo', ->(request_body) do
    #        JSON.parse(request_body).slice('name') == { 'name' => 'YK' } }) { [200, {}, '']
    #      end
    #
    #       # You can set strict_mode to exactly match the stubbed requests.
    #       stub.strict_mode = true
    #
    #       # Pass debug: true to append a diagnostics report to the
    #       # Stubs::NotFound error, showing at which step (host, path, params,
    #       # body or headers) each pending or consumed candidate was eliminated.
    #       # An object responding to #puts (e.g. $stdout) writes the report
    #       # there instead and keeps the original error message.
    #     end
    #   end
    #
    #   stubs = Faraday::Adapter::Test::Stubs.new(debug: true) do |stub|
    #     stub.get('/resource.json') { [200, {}, 'hi world'] }
    #   end
    #
    #   resp = test.get '/resource.json'
    #   resp.body # => 'hi world'
    #
    #   resp = test.get '/showget'
    #   resp.body # => 'get'
    #
    #   resp = test.get '/items/1'
    #   resp.body # => 'showing item: 1'
    #
    #   resp = test.get '/items/2'
    #   resp.body # => 'showing item: 2'
    #
    #   resp = test.post '/bar', 'name=YK&word=call'
    #   resp.status # => 200
    #
    #   resp = test.post '/foo', JSON.dump(name: 'YK', created_at: Time.now)
    #   resp.status # => 200
    class Test < Faraday::Adapter
      attr_accessor :stubs

      # A stack of Stubs
      class Stubs
        class NotFound < StandardError
        end

        # @param strict_mode [Boolean] when true, stubs match requests only when
        #   their path, parameters and headers are exactly the same.
        # @param debug [Boolean, #puts] when false (the default), a failed match
        #   raises the usual {Stubs::NotFound} error without any extra output.
        #   When true, the {Stubs::NotFound} message is extended with a
        #   diagnostics report explaining at which step each pending or already
        #   consumed candidate stub was eliminated. When given an object that
        #   responds to `#puts` (e.g. `$stdout`, `StringIO`), the report is
        #   written to it instead and the original error message is preserved.
        def initialize(strict_mode: false, debug: false)
          # { get: [Stub, Stub] }
          @stack = {}
          @consumed = {}
          @strict_mode = strict_mode
          @debug = debug
          @last_diagnostics = nil
          @stubs_mutex = Monitor.new
          yield(self) if block_given?
        end

        # @return [Stubs::Diagnostics, nil] the diagnostics collected while
        #   resolving the last request, or nil when the last request matched a
        #   stub directly or debug mode is disabled.
        attr_reader :last_diagnostics

        # @return [Boolean, #puts] the configured debug output target.
        attr_reader :debug

        def empty?
          @stack.empty?
        end

        # @param env [Faraday::Env]
        def match(env)
          request_method = env[:method]
          unless @stack.key?(request_method)
            record_diagnostics(Diagnostics.empty(env)) if @debug
            return false
          end

          stack = @stack[request_method]
          consumed = (@consumed[request_method] ||= [])

          @stubs_mutex.synchronize do
            if @debug
              match_with_diagnostics(env, request_method, stack, consumed)
            else
              stub, meta = matches?(stack, env)
              if stub
                removed = stack.delete(stub)
                consumed << removed unless removed.nil?
                return stub, meta
              end
              matches?(consumed, env)
            end
          end
        end

        def get(path, headers = {}, &block)
          new_stub(:get, path, headers, &block)
        end

        def head(path, headers = {}, &block)
          new_stub(:head, path, headers, &block)
        end

        def post(path, body = nil, headers = {}, &block)
          new_stub(:post, path, headers, body, &block)
        end

        def put(path, body = nil, headers = {}, &block)
          new_stub(:put, path, headers, body, &block)
        end

        def patch(path, body = nil, headers = {}, &block)
          new_stub(:patch, path, headers, body, &block)
        end

        def delete(path, headers = {}, &block)
          new_stub(:delete, path, headers, &block)
        end

        def options(path, headers = {}, &block)
          new_stub(:options, path, headers, &block)
        end

        # Removes all stubs, including the ones that have already been consumed.
        def clear
          @stubs_mutex.synchronize do
            @stack.clear
            @consumed.clear
          end
        end

        # Raises an error if any of the stubbed calls have not been made.
        def verify_stubbed_calls
          failed_stubs = []
          @stack.each do |method, stubs|
            next if stubs.empty?

            failed_stubs.concat(
              stubs.map do |stub|
                "Expected #{method} #{stub}."
              end
            )
          end
          raise failed_stubs.join(' ') unless failed_stubs.empty?
        end

        # Set strict_mode. If the value is true, this adapter tries to find matched requests strictly,
        # which means that all of a path, parameters, and headers must be the same as an actual request.
        def strict_mode=(value)
          @strict_mode = value
          @stack.each_value do |stubs|
            stubs.each do |stub|
              stub.strict_mode = value
            end
          end
        end

        protected

        def new_stub(request_method, path, headers = {}, body = nil, &block)
          normalized_path, host =
            if path.is_a?(Regexp)
              path
            else
              [
                Faraday::Utils.normalize_path(path),
                Faraday::Utils.URI(path).host
              ]
            end
          path, query = normalized_path.respond_to?(:split) ? normalized_path.split('?') : normalized_path
          headers = Utils::Headers.new(headers)

          stub = Stub.new(host, path, query, headers, body, @strict_mode, block)
          (@stack[request_method] ||= []) << stub
        end

        # @param stack [Hash]
        # @param env [Faraday::Env]
        def matches?(stack, env)
          stack.each do |stub|
            match_result, meta = stub.matches?(env)
            return stub, meta if match_result
          end
          nil
        end

        # Scans a stack while collecting, for every candidate stub, the detailed
        # result of every matching step. The matching logic itself is unchanged,
        # so a proc body matcher is evaluated at most once per candidate.
        #
        # @param stack [Array<Stub>]
        # @param env [Faraday::Env]
        # @param consumed [Boolean] whether the stack holds already consumed stubs.
        # @return [Array(Stub, Hash, Array)] the matched stub (or nil), its meta
        #   hash (or nil) and the collected {Diagnostics::CandidateReport}s.
        def scan(stack, env, consumed:)
          reports = stack.map do |stub|
            result, meta, breakdown = stub.match_breakdown(env)
            Diagnostics::CandidateReport.new(stub, consumed, breakdown, result, meta)
          end
          matched = reports.find(&:matched)
          [matched&.stub, matched&.meta, reports]
        end

        # Debug counterpart of {#match}. Both pending and already consumed
        # candidates are scanned so the report can distinguish them.
        def match_with_diagnostics(env, request_method, stack, consumed)
          stub, meta, pending_reports = scan(stack, env, consumed: false)
          if stub
            removed = stack.delete(stub)
            consumed << removed unless removed.nil?
            @last_diagnostics = nil
            return stub, meta
          end

          consumed_stub, consumed_meta, consumed_reports = scan(consumed, env, consumed: true)
          record_diagnostics(
            Diagnostics.new(env, request_method, pending_reports, consumed_reports)
          )
          return consumed_stub, consumed_meta if consumed_stub

          nil
        end

        def record_diagnostics(diagnostics)
          @last_diagnostics = diagnostics
          return if @debug == true

          @debug.puts(diagnostics.to_s)
        end
      end

      # Human-readable report describing why a request could not be matched.
      # Each {CandidateReport} pairs a stub with the per-step result of the
      # matching process, split between pending and already consumed stubs.
      class Diagnostics
        # @attr stub [Stub] the inspected stub.
        # @attr consumed [Boolean] whether the stub has already been consumed.
        # @attr breakdown [Array<Hash>] the per-step matching results.
        # @attr matched [Boolean] whether the stub matched the request as a whole.
        # @attr meta [Hash] the meta hash that would have been yielded to the stub.
        CandidateReport = Struct.new(:stub, :consumed, :breakdown, :matched, :meta) do
          # @return [Hash, nil] the first failing matching step, if any.
          def eliminated_at
            breakdown.find { |step| !step[:matched] }
          end
        end

        # @param env [Faraday::Env] the request that could not be matched.
        # @param request_method [Symbol] the request HTTP method.
        # @param pending_reports [Array<CandidateReport>] reports for stubs that
        #   have not been consumed yet.
        # @param consumed_reports [Array<CandidateReport>] reports for stubs that
        #   have already been consumed by previous requests.
        def initialize(env, request_method, pending_reports, consumed_reports)
          @env = env
          @request_method = request_method
          @pending_reports = pending_reports
          @consumed_reports = consumed_reports
        end

        # Builds a report for a request whose HTTP method has no stub at all.
        #
        # @param env [Faraday::Env] the unmatched request.
        # @return [Diagnostics]
        def self.empty(env)
          new(env, env[:method], [], [])
        end

        # @return [Array<CandidateReport>] reports for pending (not consumed) stubs.
        attr_reader :pending_reports

        # @return [Array<CandidateReport>] reports for already consumed stubs.
        attr_reader :consumed_reports

        # @return [String] the multi-line diagnostics report.
        def to_s
          lines = ["Faraday::Adapter::Test diagnostics for #{@request_method.to_s.upcase} #{@env[:url]}:"]
          render_reports(lines, 'Pending stubs (not consumed yet)', @pending_reports)
          render_reports(lines, 'Already consumed stubs', @consumed_reports)
          lines.join("\n")
        end

        private

        def render_reports(lines, title, reports)
          lines << "#{title} (#{reports.size}):"
          if reports.empty?
            lines << '  (none)'
            return
          end

          reports.each do |report|
            lines << "  - #{report.stub.description}"
            report.breakdown.each do |step|
              lines << "      #{step[:name]}: #{step[:matched] ? 'matched' : step[:detail]}"
            end
          end
        end
      end

      # Stub request
      Stub = Struct.new(:host, :path, :query, :headers, :body, :strict_mode, :block) do
        # @param env [Faraday::Env]
        def matches?(env)
          matched, meta, = match_breakdown(env)
          [matched, meta]
        end

        # Same matching process as {#matches?}, but keeps the result of every
        # step so a diagnostics report can show where the candidate was
        # eliminated. The steps are evaluated in order and short-circuit on the
        # first failure, which guarantees a proc body matcher runs at most once
        # and is never re-evaluated just to build the report.
        #
        # @param env [Faraday::Env]
        # @return [Array(Boolean, Hash, Array<Hash>)] whether the stub matches,
        #   the meta hash yielded to the consumer block, and the per-step results.
        def match_breakdown(env)
          meta = {}
          steps = build_breakdown(env, meta)
          [steps.all? { |step| step[:matched] }, meta, steps]
        end

        def path_match?(request_path, meta)
          if path.is_a?(Regexp)
            !!(meta[:match_data] = path.match(request_path))
          else
            path == request_path
          end
        end

        # @param env [Faraday::Env]
        def params_match?(env)
          request_params = env[:params]
          params = env.params_encoder.decode(query) || {}

          if strict_mode
            return Set.new(params) == Set.new(request_params)
          end

          params.keys.all? do |key|
            request_params[key] == params[key]
          end
        end

        def headers_match?(request_headers)
          if strict_mode
            headers_with_user_agent = headers.dup.tap do |hs|
              # NOTE: Set User-Agent in case it's not set when creating Stubs.
              #       Users would not want to set Faraday's User-Agent explicitly.
              hs[:user_agent] ||= Connection::USER_AGENT
            end
            return Set.new(headers_with_user_agent) == Set.new(request_headers)
          end

          headers.keys.all? do |key|
            request_headers[key] == headers[key]
          end
        end

        def body_match?(request_body)
          return true if body.to_s.empty?

          case body
          when Proc
            body.call(request_body)
          else
            request_body == body
          end
        end

        def to_s
          "#{path} #{body}"
        end

        # @return [String] a stable description of the stub expectation, used in
        #   diagnostics reports.
        def description
          location = path.is_a?(Regexp) ? path.inspect : path
          parts = ["#{location}"]
          parts << "?#{query}" if query && !query.to_s.empty?
          parts << "body=#{body.inspect}" unless body.to_s.empty?
          parts << "headers=#{headers.to_hash.inspect}" unless headers.empty?
          parts.join(' ')
        end

        private

        def build_breakdown(env, meta)
          request_host = env[:url].host
          request_path = Faraday::Utils.normalize_path(env[:url].path)
          request_headers = env.request_headers
          request_body = env[:body]

          step_builders = [
            -> { host_step(request_host) },
            -> { path_step(request_path, meta) },
            -> { params_step(env) },
            -> { body_step(request_body) },
            -> { headers_step(request_headers) }
          ]

          steps = []
          step_builders.each do |builder|
            step = builder.call
            steps << step
            # Stop at the first failing step, exactly like the short-circuit
            # `&&` chain used by the original matcher, so later matchers are
            # never evaluated just for the report.
            break unless step[:matched]
          end
          steps
        end

        def host_step(request_host)
          matched = host.nil? || host == request_host
          detail = "expected host #{host.inspect}, got #{request_host.inspect}" unless matched
          { name: 'host', matched: matched, detail: detail }
        end

        def path_step(request_path, meta)
          matched = path_match?(request_path, meta)
          expected = path.is_a?(Regexp) ? path.inspect : path
          detail = "expected path #{expected.inspect}, got #{request_path.inspect}" unless matched
          { name: 'path', matched: matched, detail: detail }
        end

        def params_step(env)
          matched = params_match?(env)
          expected = env.params_encoder.decode(query) || {}
          unless matched
            detail = "expected query params #{expected.inspect}, " \
                     "got #{env[:params].inspect} (strict: #{strict_mode})"
          end
          { name: 'params', matched: matched, detail: detail }
        end

        def body_step(request_body)
          # The proc is invoked here, as part of the regular matching process,
          # and never a second time when rendering the diagnostics report.
          matched = body_match?(request_body)
          detail =
            if body.is_a?(Proc)
              "proc body matcher #{body.inspect} returned false for #{request_body.inspect}"
            else
              "expected body #{body.inspect}, got #{request_body.inspect}"
            end
          { name: 'body', matched: matched, detail: matched ? nil : detail }
        end

        def headers_step(request_headers)
          matched = headers_match?(request_headers)
          if strict_mode
            expected = headers.dup.tap { |hs| hs[:user_agent] ||= Connection::USER_AGENT }.to_hash
            detail = "expected headers #{expected.inspect}, got #{request_headers.inspect} (strict)"
          else
            detail = "expected headers #{headers.to_hash.inspect}, got #{request_headers.inspect}"
          end
          { name: 'headers', matched: matched, detail: matched ? nil : detail }
        end
      end

      def initialize(app, stubs = nil, &block)
        super(app)
        @stubs = stubs || Stubs.new
        configure(&block) if block
      end

      def configure
        yield(stubs)
      end

      # @param env [Faraday::Env]
      def call(env)
        super

        env.request.params_encoder ||= Faraday::Utils.default_params_encoder
        env[:params] = env.params_encoder.decode(env[:url].query) || {}
        stub, meta = stubs.match(env)

        unless stub
          message = "no stubbed request for #{env[:method]} " \
                    "#{env[:url]} #{env[:body]} #{env[:headers]}"
          diagnostics = stubs.last_diagnostics
          message = "#{message}\n#{diagnostics}" if stubs.debug == true && diagnostics
          raise Stubs::NotFound, message
        end

        block_arity = stub.block.arity
        params = if block_arity >= 0
                   [env, meta].take(block_arity)
                 else
                   [env, meta]
                 end

        timeout = request_timeout(:open, env[:request])
        timeout ||= request_timeout(:read, env[:request])

        status, headers, body =
          if timeout
            ::Timeout.timeout(timeout, Faraday::TimeoutError) do
              stub.block.call(*params)
            end
          else
            stub.block.call(*params)
          end

        # We need to explicitly pass `reason_phrase = nil` here to avoid keyword args conflicts.
        #   See https://github.com/lostisland/faraday/issues/1444
        # TODO: remove `nil` explicit reason_phrase once Ruby 3.0 becomes minimum req. version
        save_response(env, status, body, headers, nil)

        @app.call(env)
      end
    end
  end
end

Faraday::Adapter.register_middleware(test: Faraday::Adapter::Test)
