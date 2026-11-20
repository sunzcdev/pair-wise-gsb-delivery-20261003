require_relative "../spec_helper"

describe "error_handler plugin" do 
  # A response body that tracks how many times it has been closed.
  def counting_body
    Class.new do
      attr_reader :close_count

      def initialize
        @close_count = 0
      end

      def each
        yield 'abandoned body'
      end

      def close
        @close_count += 1
      end
    end.new
  end

  it "executes only if error raised" do
    app(:bare) do
      plugin :error_handler do |e|
        e.message
      end

      route do |r|
        r.on "a" do
          "found"
        end

        raise ArgumentError, "bad idea"
      end
    end

    body("/a").must_equal 'found'
    status("/a").must_equal 200
    body.must_equal 'bad idea'
    status.must_equal 500
  end

  deprecated "works if #call is overridden" do
    app(:bare) do
      plugin :error_handler do |e|
        e.message
      end

      def call
        super
      end

      route do |r|
        r.on "a" do
          "found"
        end

        raise ArgumentError, "bad idea"
      end
    end

    body("/a").must_equal 'found'
    status("/a").must_equal 200
    body.must_equal 'bad idea'
    status.must_equal 500
  end

  it "executes on SyntaxError exceptions" do
    app(:bare) do
      plugin :error_handler do |e|
        e.message
      end

      route do |r|
        r.on "a" do
          "found"
        end

        raise SyntaxError, 'bad idea'
      end
    end

    body("/a").must_equal 'found'
    status("/a").must_equal 200
    body.must_equal 'bad idea'
    status.must_equal 500
  end

  it "executes on custom exception classes" do
    app(:bare) do
      plugin :error_handler, :classes=>[StandardError] do |e|
        e.message
      end

      route do |r|
        r.on "a" do
          raise 'foo'
        end

        raise SyntaxError, 'bad idea'
      end
    end

    proc{body}.must_raise SyntaxError
    body("/a").must_equal 'foo'

    @app = Class.new(@app)
    proc{body}.must_raise SyntaxError
    body("/a").must_equal 'foo'
  end

  it "can override status inside error block" do
    app(:bare) do
      plugin :error_handler do |e|
        response.status = 501
        e.message
      end

      route do |r|
        raise ArgumentError, "bad idea"
      end
    end

    status.must_equal 501
  end

  it "calculates correct Content-Length" do
    app(:bare) do
      plugin :error_handler do |e|
        "a"
      end

      route do |r|
        raise ArgumentError, "bad idea"
      end
    end

    header(RodaResponseHeaders::CONTENT_LENGTH).must_equal "1"
  end

  deprecated "supports class-level error method for setting error handler" do
    app(:bare) do
      plugin :error_handler

      error do |e|
        "a"
      end

      route do |r|
        raise ArgumentError, "bad idea"
      end
    end

    header(RodaResponseHeaders::CONTENT_LENGTH).must_equal "1"
  end

  it "clears existing headers" do
    app(:bare) do
      plugin :error_handler do |e|
        "a"
      end

      route do |r|
        response[RodaResponseHeaders::CONTENT_TYPE] = 'text/pdf'
        response['foo'] = 'bar'
        raise ArgumentError, "bad idea"
      end
    end

    header(RodaResponseHeaders::CONTENT_TYPE).must_equal 'text/html'
    header('foo').must_be_nil
  end

  it "can set error via the plugin block" do
    app(:bare) do
      plugin :error_handler do |e|
        e.message
      end

      route do |r|
        raise ArgumentError, "bad idea"
      end
    end

    body.must_equal 'bad idea'
  end

  it "has default error handler also raise" do
    app(:bare) do
      plugin :error_handler

      route do |r|
        raise ArgumentError, "bad idea"
      end
    end

    proc{req}.must_raise(ArgumentError)
  end

  it "logs exceptions during after processing of error handler" do
    app(:bare) do
      plugin :error_handler do |e|
        e.message * 2
      end
      plugin :hooks

      after do |_|
        raise "foo"
      end

      route do |r|
        ''
      end
    end

    errors = rack_input
    body('rack.errors'=>errors).must_equal 'foofoo'
    errors.rewind
    errors.read.split("\n").first.must_equal "Error in after hook processing of error handler: RuntimeError: foo"
  end

  it "has access to current remaining_path" do
    app(:bare) do
      plugin :error_handler do |e|
        request.remaining_path
      end

      route do |r|
        r.on('a') do
          raise ArgumentError, "bad idea"
        end

        raise ArgumentError, "bad idea"
      end
    end

    body.must_equal '/'
    body('/b').must_equal '/b'
    body('/a').must_equal ''
    body('/a/c').must_equal '/c'
  end

  it "closes the abandoned response body exactly once when the error handler recovers" do
    abandoned = counting_body
    handler_calls = 0
    after_calls = 0

    app(:bare) do
      plugin :error_handler do |e|
        handler_calls += 1
        response.status = 503
        response['x-handler'] = 'handler'
        "handled: #{e.message}"
      end

      plugin :hooks

      after do |_|
        after_calls += 1
        raise "after hook failure" if after_calls == 1
      end

      route do |r|
        response.status = 201
        response['x-abandoned'] = 'abandoned'
        response[RodaResponseHeaders::CONTENT_TYPE] = 'text/plain'
        r.halt [201, response.headers, abandoned]
      end
    end

    res = req
    res[0].must_equal 503
    b = res[2]
    s = String.new
    b.each{|x| s << x}
    b.close if b.respond_to?(:close)
    s.must_equal 'handled: after hook failure'

    # The abandoned body is closed exactly once.
    abandoned.close_count.must_equal 1

    # The abandoned status and headers are not used for the error response.
    res[1]['x-abandoned'].must_be_nil
    res[1]['x-handler'].must_equal 'handler'
    res[1][RodaResponseHeaders::CONTENT_TYPE].must_equal 'text/html'

    # The error handler is not called again for exceptions raised in its
    # after processing, and normal after processing runs for the error response.
    handler_calls.must_equal 1
    after_calls.must_equal 2
  end

  it "closes the abandoned response body exactly once and does not reenter the error handler when the error handler raises" do
    abandoned = counting_body
    handler_calls = 0
    after_calls = 0

    app(:bare) do
      plugin :error_handler do |e|
        handler_calls += 1
        raise ArgumentError, "handler failure"
      end

      plugin :hooks

      after do |_|
        after_calls += 1
        raise "after hook failure" if after_calls == 1
      end

      route do |r|
        r.halt [201, {'x-abandoned'=>'abandoned'}, abandoned]
      end
    end

    e = proc{req}.must_raise(ArgumentError)
    e.message.must_equal 'handler failure'

    # The abandoned body is closed exactly once.
    abandoned.close_count.must_equal 1

    # The error handler fails once and is not called again for its own exception.
    handler_calls.must_equal 1
    after_calls.must_equal 1

    # On Ruby 2.1+, the exception raised by the error handler keeps the original
    # exception as its cause, so both can be located.
    if e.respond_to?(:cause) && RUBY_VERSION >= '2.1'
      e.cause.must_be_instance_of RuntimeError
      e.cause.message.must_equal 'after hook failure'
    end
  end

  it "does not reenter the error handler when the error handler raises, and preserves an existing cause" do
    handler_calls = 0
    original = RuntimeError.new('original')
    wrapped = begin
      raise 'wrapped', cause: original
    rescue RuntimeError => e
      e
    end

    app(:bare) do
      plugin :error_handler do |_|
        handler_calls += 1
        raise wrapped
      end

      route do |r|
        raise 'routing failure'
      end
    end

    e = proc{req}.must_raise(RuntimeError)
    e.must_be_same_as wrapped
    handler_calls.must_equal 1
    e.cause.must_be_same_as original
  end

  it "does not close a body that does not respond to close when abandoning a response" do
    app(:bare) do
      plugin :error_handler do |e|
        'handled'
      end

      plugin :hooks

      calls = 0
      after do |_|
        calls += 1
        raise 'after hook failure' if calls == 1
      end

      route do |r|
        r.halt [201, {}, ['abandoned']]
      end
    end

    body.must_equal 'handled'
  end
end
