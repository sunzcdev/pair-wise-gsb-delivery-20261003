require_relative "../spec_helper"

describe "response_complete plugin" do
  # Body that yields chunks one at a time, can be configured to fail
  # midway or when closed, and records the order of events.
  class ResponseCompleteSpecBody
    def initialize(events, chunks, fail_at: nil, close_error: nil)
      @events = events
      @chunks = chunks
      @fail_at = fail_at
      @close_error = close_error
    end

    def each
      @chunks.each_with_index do |chunk, i|
        raise "mid-iteration failure" if i == @fail_at
        @events << [:chunk, chunk]
        yield chunk
      end
    end

    def close
      @events << [:body_close]
      raise @close_error if @close_error
    end
  end

  before do
    events = @events = []
    holder = @holder = {}
    app(:bare) do
      plugin :response_complete

      response_complete do |result|
        events << [:hook, result, @resource]
      end

      route do |r|
        @resource = "resource"
        r.on 'array' do
          'foo'
        end
        r.halt response.finish_with_body(holder[:body])
      end
    end
  end

  it "does not call hook when response is returned, calls hook with :complete after array body is fully sent, and not again on close" do
    s, h, b = req('/array')
    s.must_equal 200
    @events.must_equal []
    chunks = []
    b.each{|c| chunks << c}
    chunks.must_equal ['foo']
    @events.must_equal [[:hook, :complete, 'resource']]
    b.close
    b.close
    @events.must_equal [[:hook, :complete, 'resource']]
  end

  it "yields lazy body chunks in order before calling hook with :complete, and closes original body" do
    @holder[:body] = ResponseCompleteSpecBody.new(@events, %w[a b c])
    s, h, b = req
    @events.must_equal []
    chunks = []
    b.each{|c| chunks << c}
    chunks.must_equal %w[a b c]
    @events.must_equal [[:chunk, 'a'], [:chunk, 'b'], [:chunk, 'c'], [:hook, :complete, 'resource']]
    b.close
    @events.must_equal [[:chunk, 'a'], [:chunk, 'b'], [:chunk, 'c'], [:hook, :complete, 'resource'], [:body_close]]
    b.close
    @events.must_equal [[:chunk, 'a'], [:chunk, 'b'], [:chunk, 'c'], [:hook, :complete, 'resource'], [:body_close], [:body_close]]
  end

  it "calls hook with :abort and reraises when body fails midway" do
    @holder[:body] = ResponseCompleteSpecBody.new(@events, %w[a b c], fail_at: 1)
    s, h, b = req
    @events.must_equal []
    proc{b.each{|c| }}.must_raise(RuntimeError)
    @events.must_equal [[:chunk, 'a'], [:hook, :abort, 'resource']]
    b.close
    @events.must_equal [[:chunk, 'a'], [:hook, :abort, 'resource'], [:body_close]]
  end

  it "calls hook with :abort when server closes body without enumerating it" do
    @holder[:body] = ResponseCompleteSpecBody.new(@events, %w[a b c])
    s, h, b = req
    b.close
    @events.must_equal [[:body_close], [:hook, :abort, 'resource']]
    b.close
    @events.must_equal [[:body_close], [:hook, :abort, 'resource'], [:body_close]]
  end

  it "calls hook with :abort when consumer breaks out of enumeration early" do
    @holder[:body] = ResponseCompleteSpecBody.new(@events, %w[a b c])
    s, h, b = req
    b.each{|c| break}
    @events.must_equal [[:chunk, 'a'], [:hook, :abort, 'resource']]
  end

  it "propagates exceptions raised by the original body's close, and still settles" do
    @holder[:body] = ResponseCompleteSpecBody.new(@events, %w[a], close_error: "close failure")
    s, h, b = req
    b.each{|c| }
    @events.must_equal [[:chunk, 'a'], [:hook, :complete, 'resource']]
    proc{b.close}.must_raise(RuntimeError)
    @events.must_equal [[:chunk, 'a'], [:hook, :complete, 'resource'], [:body_close]]
  end

  it "calls all registered hooks exactly once" do
    other = []
    @app.response_complete{|result| other << result}
    s, h, b = req('/array')
    b.each{|c| }
    b.close
    @events.must_equal [[:hook, :complete, 'resource']]
    other.must_equal [:complete]
  end

  it "does not wrap the body if no hooks are defined" do
    app(:bare) do
      plugin :response_complete
      route do |r|
        'foo'
      end
    end
    s, h, b = req
    b.must_equal ['foo']
  end

  it "works when freezing the app" do
    app.freeze
    s, _, b = req('/array')
    b.each{|c| }
    @events.must_equal [[:hook, :complete, 'resource']]
  end

  it "copies hooks when subclassing" do
    @app = Class.new(@app)
    s, _, b = req('/array')
    b.each{|c| }
    @events.must_equal [[:hook, :complete, 'resource']]
  end

  it "wraps bodies returned by the error handler" do
    events = @events
    app(:bare) do
      plugin :response_complete
      plugin(:error_handler){|e| 'error'}

      response_complete do |result|
        events << [:hook, result]
      end

      route do |r|
        raise 'foo'
      end
    end
    s, h, b = req
    s.must_equal 500
    @events.must_equal []
    chunks = []
    b.each{|c| chunks << c}
    chunks.must_equal ['error']
    @events.must_equal [[:hook, :complete]]
  end
end
