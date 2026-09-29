require_relative 'test_helper'

class SSETest < Minitest::Test
  SSE = Sinatra::SSE

  it 'encodes data, id, event and retry fields into one frame' do
    sse = SSE.new { |out| out.event 'hello', id: 7, event: 'greeting', retry: 3000 }
    assert_equal "event: greeting\nid: 7\nretry: 3000\ndata: hello\n\n", sse.each.to_a.join
  end

  it 'omits fields that were not given' do
    sse = SSE.new { |out| out.event 'hi' }
    assert_equal "data: hi\n\n", sse.each.to_a.join
  end

  it 'splits multi-line data into one data line per line' do
    sse = SSE.new { |out| out.event "a\nb\r\nc\rd" }
    assert_equal "data: a\ndata: b\ndata: c\ndata: d\n\n", sse.each.to_a.join
  end

  it 'encodes comments as colon-prefixed frames' do
    sse = SSE.new { |out| out.comment 'keep-alive' }
    assert_equal ": keep-alive\n\n", sse.each.to_a.join
  end

  it 'rejects newline injection in the event field' do
    sse = SSE.new { |out| out.event 'ok', event: "msg\ndata: injected" }
    assert_raises(ArgumentError) { sse.each { } }
  end

  it 'rejects newline injection in the id field' do
    sse = SSE.new { |out| out.event 'ok', id: "1\r\nretry: 0" }
    assert_raises(ArgumentError) { sse.each { } }
  end

  it 'rejects newline injection in comments' do
    sse = SSE.new { |out| out.comment "ping\ndata: injected" }
    assert_raises(ArgumentError) { sse.each { } }
  end

  it 'rejects retry values that are not non-negative integers' do
    sse = SSE.new { |out| out.event 'ok', retry: '10; anything' }
    assert_raises(ArgumentError) { sse.each { } }

    negative = SSE.new { |out| out.event 'ok', retry: -1 }
    assert_raises(ArgumentError) { negative.each { } }
  end

  it 'only produces the frames the consumer pulls and cancels the producer on close' do
    produced = 0
    cancelled = false
    sse = SSE.new do |out|
      begin
        loop do
          produced += 1
          out.event "tick-#{produced}"
        end
      rescue SSE::Cancelled
        cancelled = true
      end
    end

    frames = []
    sse.each do |frame|
      frames << frame
      break if frames.size == 2
    end
    sse.close

    assert_equal ["data: tick-1\n\n", "data: tick-2\n\n"], frames
    assert_equal 2, produced
    assert cancelled, 'producer should receive the cancellation signal'
    assert sse.closed?
  end

  it 'does not run any production work before the first pull' do
    produced = 0
    SSE.new { |_out| produced += 1 }
    assert_equal 0, produced
  end

  it 'lets producers clean up via ensure when cancelled' do
    cleaned = false
    sse = SSE.new do |out|
      begin
        out.event 'a'
        out.event 'b'
      ensure
        cleaned = true
      end
    end

    sse.each { |_| break }
    sse.close
    assert cleaned
  end

  it 'emits heartbeats based on the injected clock' do
    time = 0.0
    clock = -> { time }
    beats = []
    sse = SSE.new(clock: clock, heartbeat_interval: 15) do |out|
      beats << out.heartbeat
      time = 20.0
      beats << out.heartbeat
      out.event 'x'
      time = 30.0
      beats << out.heartbeat
    end

    assert_equal [": heartbeat\n\n", "data: x\n\n"], sse.each.to_a
    assert_equal [false, true, false], beats
  end

  it 'does not start any background threads' do
    before = Thread.list
    sse = SSE.new { |out| 3.times { |n| out.event n } }
    sse.each { }
    sse.close
    assert_equal before, Thread.list
  end

  it 'is safe to close more than once and before any pull' do
    sse = SSE.new { |out| out.event 'x' }
    sse.close
    sse.close
    assert sse.closed?
    assert_equal [], sse.each.to_a
  end

  it 'requires a producer block' do
    assert_raises(ArgumentError) { SSE.new }
  end

  it 'streams SSE responses from routes' do
    mock_app do
      get('/') do
        sse do |out|
          out.comment 'ready'
          out.event 'hi', id: 1, event: 'msg'
        end
      end
    end
    get '/'
    assert_include response.content_type, 'text/event-stream'
    assert_equal ": ready\n\nevent: msg\nid: 1\ndata: hi\n\n", body
  end
end
