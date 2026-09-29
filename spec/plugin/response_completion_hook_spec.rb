require_relative "../spec_helper"

describe "response_completion_hook plugin" do
  # A response body that lazily yields chunks, can be configured to
  # fail partway through, and records the order of events in a log.
  class ChunkBody
    def initialize(chunks, log, fail_at: nil)
      @chunks = chunks
      @log = log
      @fail_at = fail_at
      @closes = 0
    end

    def each
      @chunks.each_with_index do |chunk, i|
        if @fail_at == i
          @log << [:body_error, chunk]
          raise Roda::RodaError, "failure at chunk #{i}"
        end
        @log << [:chunk, chunk]
        yield chunk
      end
    end

    def close
      @closes += 1
      @log << [:body_close, @closes]
    end
  end

  def chunk_app(log, chunks, fail_at: nil)
    app(:response_completion_hook) do |r|
      response.on_completion do |state, error|
        log << [:hook, state, error && error.message]
      end
      throw :halt, response.finish_with_body(ChunkBody.new(chunks, log, fail_at: fail_at))
    end
  end

  it "calls hook with :complete after a normal array body is fully sent" do
    unless_lint do
      log = []
      app(:response_completion_hook) do |r|
        response.on_completion{|state, error| log << [:hook, state, error]}
        response.write('a')
        response.write('b')
      end

      s, _, b = req
      s.must_equal 200
      log.must_equal []
      received = []
      b.each{|x| received << x}
      received.must_equal %w'a b'
      log.must_equal [[:hook, :complete, nil]]
      b.close
      log.must_equal [[:hook, :complete, nil]]
    end
  end

  it "does not wrap the body or call hooks when no hook is registered" do
    app(:response_completion_hook) do |r|
      response.write('a')
    end

    req[2].must_equal ['a']
  end

  it "yields chunks in order and calls hook with :complete for a lazy enumerable body" do
    unless_lint do
      log = []
      chunk_app(log, %w'a b c')

      s, _, b = req
      s.must_equal 200
      log.must_equal []
      received = []
      b.each{|x| received << x}
      received.must_equal %w'a b c'
      log.must_equal [[:chunk, 'a'], [:chunk, 'b'], [:chunk, 'c'], [:hook, :complete, nil]]
      b.close
      log.must_equal [[:chunk, 'a'], [:chunk, 'b'], [:chunk, 'c'], [:hook, :complete, nil], [:body_close, 1]]
    end
  end

  it "calls hook with :aborted and reraises when the body fails partway through" do
    unless_lint do
      log = []
      chunk_app(log, %w'a b c', fail_at: 1)

      _, _, b = req
      received = []
      proc{b.each{|x| received << x}}.must_raise(Roda::RodaError)
      received.must_equal %w'a'
      log.must_equal [[:chunk, 'a'], [:body_error, 'b'], [:hook, :aborted, 'failure at chunk 1']]

      # The server still closes the body after the error; the original
      # body's close obligation is preserved, but the hook is not
      # called again.
      b.close
      log.must_equal [[:chunk, 'a'], [:body_error, 'b'], [:hook, :aborted, 'failure at chunk 1'], [:body_close, 1]]
    end
  end

  it "calls hook with :aborted when the server closes the body mid-stream" do
    unless_lint do
      log = []
      chunk_app(log, %w'a b c')

      _, _, b = req
      enum = b.to_enum(:each)
      enum.next.must_equal 'a'
      b.close
      log.must_equal [[:chunk, 'a'], [:hook, :aborted, nil], [:body_close, 1]]
    end
  end

  it "calls hook with :aborted when the server closes the body without enumerating it" do
    unless_lint do
      log = []
      chunk_app(log, %w'a b c')

      _, _, b = req
      b.close
      log.must_equal [[:hook, :aborted, nil], [:body_close, 1]]
    end
  end

  it "only settles once when close is called repeatedly" do
    unless_lint do
      log = []
      chunk_app(log, %w'a b c')

      _, _, b = req
      b.close
      b.close
      b.close
      log.must_equal [[:hook, :aborted, nil], [:body_close, 1]]
    end
  end

  it "supports multiple hooks, each called once in registration order" do
    unless_lint do
      log = []
      app(:response_completion_hook) do |r|
        response.on_completion{|state,| log << [:first, state]}
        response.on_completion{|state,| log << [:second, state]}
        response.write('a')
      end

      _, _, b = req
      b.each{|x|}
      log.must_equal [[:first, :complete], [:second, :complete]]
      b.close
      log.must_equal [[:first, :complete], [:second, :complete]]
    end
  end

  it "raises if on_completion is called without a block" do
    app(:response_completion_hook) do |r|
      response.on_completion
    end

    proc{req}.must_raise(Roda::RodaError)
  end

  it "does not call hooks for responses returned by throwing a rack array with halt" do
    log = []
    app(:response_completion_hook) do |r|
      response.on_completion{|state,| log << state}
      throw :halt, [200, {}, ['a']]
    end

    body.must_equal 'a'
    log.must_equal []
  end
end
