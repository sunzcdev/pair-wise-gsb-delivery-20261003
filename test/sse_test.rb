require_relative 'test_helper'

class SSETest < Minitest::Test
  SSE = Sinatra::SSE

  it 'encodes data, id, event, and retry fields' do
    sse = SSE.new do |stream|
      stream.event('hello', id: 42, event: 'message', retry: 1500)
    end

    frame = nil
    sse.each { |event_frame| frame = event_frame }

    assert_equal "data: hello\nid: 42\nevent: message\nretry: 1500\n\n", frame
  end

  it 'encodes multiline data as multiple data lines' do
    sse = SSE.new do |stream|
      stream.event("first\nsecond\r\nthird\r\n")
    end

    frame = nil
    sse.each { |event_frame| frame = event_frame }

    assert_equal "data: first\ndata: second\ndata: third\ndata: \n\n", frame
  end

  it 'rejects line breaks in non-data fields' do
    assert_field_rejected(:id, "1\nx")
    assert_field_rejected(:event, "message\r\nx")
    assert_field_rejected(:comment, "note\nx")
  end

  it 'validates the retry field' do
    assert_raises(ArgumentError) { build_stream.event('x', retry: -1) }
    assert_raises(ArgumentError) { build_stream.event('x', retry: '15\n00') }
  end

  it 'only produces frames when the Rack body is pulled' do
    produced = 0
    sse = SSE.new do |stream|
      3.times do |index|
        break if stream.closed?

        produced += 1
        stream.event(index)
      end
    end

    frames = []

    assert_equal 0, produced
    sse.each do |frame|
      frames << frame
      break if frames.size == 2
    end

    bytes = frames.join
    assert_equal 2, produced
    assert_equal 2, frames.size
    assert_equal 2, produced
    assert_equal "data: 0\n\ndata: 1\n\n", bytes
  end

  it 'notifies the producer when the body is closed' do
    canceled = false
    sse = SSE.new do |stream|
      stream.on_cancel { canceled = true }

      3.times do |index|
        break if stream.closed?

        stream.event(index)
      end
    end

    frame_count = 0
    sse.each do
      frame_count += 1
      break if frame_count == 2
    end

    assert canceled
    assert sse.closed?
  end

  it 'passes cancellation to the producer when body.close is called' do
    canceled = false
    sse = SSE.new do |stream|
      stream.on_cancel { canceled = true }

      3.times do |index|
        break if stream.closed?

        stream.event(index)
      end
    end

    frames = []
    sse.each do |frame|
      frames << frame
      sse.close if frames.size == 2
    end

    assert_equal 2, frames.size
    assert_equal 2, frames.join.scan(/\n\n/).size
    assert canceled
  end

  it 'does not start a background thread' do
    threads_before = Thread.list.size
    sse = SSE.new { |stream| stream.event('x') }

    assert_equal threads_before, Thread.list.size

    sse.each { }
    assert_equal threads_before, Thread.list.size
  end

  it 'encodes comments and explicit heartbeats' do
    sse = SSE.new do |stream|
      stream.comment('connected')
      stream.comment('')
      stream.heartbeat
    end

    frames = []
    sse.each { |frame| frames << frame }

    assert_equal [
      ": connected\n\n",
      ":\n\n",
      ": heartbeat\n\n"
    ], frames
  end

  it 'sends automatic heartbeats with an injected clock' do
    clock = Object.new
    current = 0
    clock.define_singleton_method(:now) { current }

    sse = SSE.new(heartbeat: 10, clock: clock) do |stream|
      stream.event('first')
      stream.event('second')
    end

    frames = []
    frame_count = 0
    sse.each do |frame|
      frames << frame
      frame_count += 1

      current = 9 if frame_count == 1
      current = 19 if frame_count == 2

      break if frame_count == 3
    end

    assert_equal [
      "data: first\n\n",
      "data: second\n\n",
      ": heartbeat\n\n"
    ], frames
  end

  it 'still resumes the producer after an automatic heartbeat' do
    clock = Object.new
    current = 10
    clock.define_singleton_method(:now) { current }

    sse = SSE.new(heartbeat: 0, clock: clock) do |stream|
      stream.event('first')
      stream.event('second')
    end

    frames = []
    sse.each do |frame|
      frames << frame
      break if frames.size == 3
    end

    assert_equal [
      ": heartbeat\n\n",
      "data: first\n\n",
      ": heartbeat\n\n"
    ], frames
  end

  it 'sets the SSE content type through the helper' do
    mock_app do
      get('/') do
        sse do |stream|
          stream.event('hello')
        end
      end
    end

    status, headers, body = app.call(Rack::MockRequest.env_for('/'))

    assert_equal 200, status
    assert_equal 'text/event-stream', headers['content-type']
    assert_nil headers['content-length']
    frames = []
    body.each { |frame| frames << frame }

    assert_equal ["data: hello\n\n"], frames
  ensure
    body&.close
  end

  it 'keeps route params while producing later frames' do
    mock_app do
      get('/:name') do
        sse do |stream|
          stream.event(params[:name])
          stream.event(params[:name])
        end
      end
    end

    _status, _headers, body = app.call(Rack::MockRequest.env_for('/frank'))
    frames = []
    body.each { |frame| frames << frame }

    assert_equal ["data: frank\n\n", "data: frank\n\n"], frames
  ensure
    body&.close
  end

  private

  def build_stream
    SSE.new { }
  end

  def assert_field_rejected(field, value)
    sse = SSE.new do |stream|
      case field
      when :id then stream.event('x', id: value)
      when :event then stream.event('x', event: value)
      when :comment then stream.comment(value)
      end
    end

    assert_raises(ArgumentError) do
      sse.each { }
    end
  end
end
