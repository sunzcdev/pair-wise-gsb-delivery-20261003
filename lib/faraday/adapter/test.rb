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
    #     end
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
    #
    # A stub can also return a {Faraday::Adapter::Test::Stream} to describe the
    # response body as an ordered list of chunks. When the request sets
    # `req.options.on_data`, each chunk is delivered through Env's streaming
    # callback in order, while status and ordinary response headers are still
    # saved up front.
    #
    #   stub.get '/stream' do
    #     Faraday::Adapter::Test::Stream.new(
    #       200,
    #       { 'Content-Type' => 'text/plain' },
    #       ['hel', 'lo ', 'world']
    #     )
    #   end
    #
    # Pass an error instead of a chunk to break the stream mid-way and surface
    # that error to the caller through the normal adapter error path.
    class Test < Faraday::Adapter
      attr_accessor :stubs

      # Explicit, ordered description of a streamed response body.
      #
      # Return a Stream from a stub block (instead of the status/headers/body
      # triple) to drive the request's `on_data` callback chunk by chunk. An
      # element that is an Exception breaks the stream at that point and is
      # raised through the adapter, leaving the chunks delivered before it
      # observable.
      #
      # @example three chunks
      #   Stream.new(200, { 'Content-Type' => 'text/plain' }, %w[hel lo world])
      #
      # @example an empty stream
      #   Stream.new(200, {}, [])
      #
      # @example break after the first chunk
      #   Stream.new(200, {}, ['first', Faraday::ConnectionFailed.new('boom')])
      class Stream
        # @return [Integer] HTTP response status code.
        attr_reader :status

        # @return [Hash] ordinary response headers, saved before streaming.
        attr_reader :headers

        # @return [Array<String, Exception>] ordered chunks; an Exception
        #   element breaks the stream when it is reached.
        attr_reader :chunks

        # @return [Array<String>] chunks already delivered to the callback.
        attr_reader :delivered

        # @return [Exception, nil] the Exception chunk that broke the stream,
        #   or nil when the stream has not errored.
        attr_reader :error

        # @param status [Integer] HTTP response status code.
        # @param headers [Hash] response headers.
        # @param chunks [Array<String, Exception>] ordered body chunks.
        def initialize(status, headers = {}, chunks = [])
          @status = status
          @headers = headers
          @chunks = chunks
          @delivered = []
          @error = nil
          @finished = false
        end

        # Drives Env's streaming callback with each chunk in order. Headers and
        # status are already saved by the adapter. If an Exception chunk is
        # reached, it is recorded and re-raised so the adapter's error path is
        # exercised.
        #
        # @param env [Faraday::Env] the current request environment.
        # @yield [chunk] block expected by {Env#stream_response}.
        # @return [void]
        def each_chunk(env)
          env.stream_response do |writer|
            chunks.each do |chunk|
              if chunk.is_a?(Exception)
                @error = chunk
                raise chunk
              end

              writer.call(chunk)
              @delivered << chunk
            end
          end
        ensure
          @finished = error.nil?
        end

        # @return [Boolean] true once every chunk has been delivered without
        #   an error.
        def finished?
          @finished
        end

        # @return [Integer] number of chunks delivered so far.
        def chunks_delivered
          @delivered.length
        end

        # @return [Integer] total number of bytes delivered so far.
        def bytes_delivered
          @delivered.sum(0) { |chunk| chunk.bytesize }
        end
      end

      # A stack of Stubs
      class Stubs
        class NotFound < StandardError
        end

        def initialize(strict_mode: false)
          # { get: [Stub, Stub] }
          @stack = {}
          @consumed = {}
          @strict_mode = strict_mode
          @stubs_mutex = Monitor.new
          yield(self) if block_given?
        end

        def empty?
          @stack.empty?
        end

        # @param env [Faraday::Env]
        def match(env)
          request_method = env[:method]
          return false unless @stack.key?(request_method)

          stack = @stack[request_method]
          consumed = (@consumed[request_method] ||= [])

          @stubs_mutex.synchronize do
            stub, meta = matches?(stack, env)
            if stub
              removed = stack.delete(stub)
              consumed << removed unless removed.nil?
              return stub, meta
            end
          end
          matches?(consumed, env)
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
      end

      # Stub request
      Stub = Struct.new(:host, :path, :query, :headers, :body, :strict_mode, :block) do
        # @param env [Faraday::Env]
        def matches?(env)
          request_host = env[:url].host
          request_path = Faraday::Utils.normalize_path(env[:url].path)
          request_headers = env.request_headers
          request_body = env[:body]

          # meta is a hash used as carrier
          # that will be yielded to consumer block
          meta = {}
          [(host.nil? || host == request_host) &&
            path_match?(request_path, meta) &&
            params_match?(env) &&
            body_match?(request_body) &&
            headers_match?(request_headers), meta]
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
          raise Stubs::NotFound, "no stubbed request for #{env[:method]} " \
                                 "#{env[:url]} #{env[:body]} #{env[:headers]}"
        end

        block_arity = stub.block.arity
        params = if block_arity >= 0
                   [env, meta].take(block_arity)
                 else
                   [env, meta]
                 end

        timeout = request_timeout(:open, env[:request])
        timeout ||= request_timeout(:read, env[:request])

        result =
          if timeout
            ::Timeout.timeout(timeout, Faraday::TimeoutError) do
              stub.block.call(*params)
            end
          else
            stub.block.call(*params)
          end

        if result.is_a?(Stream)
          handle_stream(env, result)
        else
          status, headers, body = result
          # We need to explicitly pass `reason_phrase = nil` here to avoid keyword args conflicts.
          #   See https://github.com/lostisland/faraday/issues/1444
          # TODO: remove `nil` explicit reason_phrase once Ruby 3.0 becomes minimum req. version
          save_response(env, status, body, headers, nil)
        end

        @app.call(env)
      end

      private

      # Streams a {Stream} stub response. When the request registered an
      # `on_data` callback we mirror a real streaming adapter: save status and
      # headers without finishing the response, push the chunks through Env's
      # streaming callback, then finish it. Without a callback the chunks are
      # concatenated and the response is saved normally, so Stream stubs also
      # work for ordinary (non-streaming) requests.
      #
      # @param env [Faraday::Env]
      # @param stream [Stream]
      def handle_stream(env, stream)
        if env.stream_response?
          # Save status and headers up front but leave the response unfinished
          # until every chunk has been delivered, matching real adapters.
          save_response(env, stream.status, nil, stream.headers, nil, finished: false)
          stream.each_chunk(env)
          env.response_body = +''
          env.response.finish(env)
        else
          body = stream.chunks.reject { |chunk| chunk.is_a?(Exception) }.join
          save_response(env, stream.status, body, stream.headers, nil)
        end
      end
    end
  end
end

Faraday::Adapter.register_middleware(test: Faraday::Adapter::Test)
