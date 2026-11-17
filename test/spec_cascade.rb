# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/cascade'
  require_relative '../lib/rack/lint'
  require_relative '../lib/rack/mock_request'
  require_relative '../lib/rack/urlmap'
  require_relative '../lib/rack/files'
end

describe Rack::Cascade do
  def cascade(*args)
    Rack::Lint.new Rack::Cascade.new(*args)
  end

  docroot = File.expand_path(File.dirname(__FILE__))
  app1 = Rack::Files.new(docroot)

  app2 = Rack::URLMap.new("/crash" => lambda { |env| raise "boom" })

  app3 = Rack::URLMap.new("/foo" => lambda { |env|
                            [200, { "content-type" => "text/plain" }, [""]]})

  it "dispatch onward on 404 and 405 by default" do
    cascade = cascade([app1, app2, app3])
    Rack::MockRequest.new(cascade).get("/cgi/test").must_be :ok?
    Rack::MockRequest.new(cascade).get("/foo").must_be :ok?
    Rack::MockRequest.new(cascade).get("/toobad").must_be :not_found?
    Rack::MockRequest.new(cascade).get("/cgi/../..").must_be :client_error?

    # Put is not allowed by Rack::Files so it'll 405.
    Rack::MockRequest.new(cascade).put("/foo").must_be :ok?
  end

  it "dispatch onward on whatever is passed" do
    cascade = cascade([app1, app2, app3], [404, 403])
    Rack::MockRequest.new(cascade).get("/cgi/../bla").must_be :not_found?
  end

  it "include? returns whether app is included" do
    cascade = Rack::Cascade.new([app1, app2])
    cascade.include?(app1).must_equal true
    cascade.include?(app2).must_equal true
    cascade.include?(app3).must_equal false
  end

  it "return 404 if empty" do
    Rack::MockRequest.new(cascade([])).get('/').must_be :not_found?
  end

  it "uses new response object if empty" do
    app = Rack::Cascade.new([])
    res = app.call('/')
    s, h, body = res
    s.must_equal 404
    h['content-type'].must_equal 'text/plain'
    body.must_be_empty

    res[0] = 200
    h['content-type'] = 'text/html'
    body << "a"

    res = app.call('/')
    s, h, body = res
    s.must_equal 404
    h['content-type'].must_equal 'text/plain'
    body.must_be_empty
  end

  it "returns final response if all responses are cascaded" do
   app = Rack::Cascade.new([])
   app << lambda { |env| [405, {}, []] }
   app.call({})[0].must_equal 405
  end

  it "append new app" do
    cascade = Rack::Cascade.new([], [404, 403])
    Rack::MockRequest.new(cascade).get('/').must_be :not_found?
    cascade << app2
    Rack::MockRequest.new(cascade).get('/cgi/test').must_be :not_found?
    Rack::MockRequest.new(cascade).get('/cgi/../bla').must_be :not_found?
    cascade << app1
    Rack::MockRequest.new(cascade).get('/cgi/test').must_be :ok?
    Rack::MockRequest.new(cascade).get('/cgi/../..').must_be :client_error?
    Rack::MockRequest.new(cascade).get('/foo').must_be :not_found?
    cascade << app3
    Rack::MockRequest.new(cascade).get('/foo').must_be :ok?
  end

  it "close the body on cascade" do
    body = StringIO.new
    closer = lambda { |env| [404, {}, body] }
    cascade = Rack::Cascade.new([closer, app3], [404])
    Rack::MockRequest.new(cascade).get("/foo").must_be :ok?
    body.must_be :closed?
  end

  it "does not alter the environment in the default mode" do
    seen = []
    first = lambda { |env| seen << env; env['PATH_INFO'] = '/changed'; [404, {}, []] }
    second = lambda { |env| seen << env; [200, {}, ["ok"]] }
    cascade = Rack::Cascade.new([first, second])

    env = Rack::MockRequest.env_for("/foo")
    cascade.call(env)

    # Both apps receive the very same environment object.
    seen[0].must_be_same_as seen[1]
    # Historical behavior: mutations by an unaccepted app leak through.
    seen[1]['PATH_INFO'].must_equal '/changed'
  end

  it "isolates environment mutations when isolate is enabled" do
    seen = []
    first = lambda do |env|
      seen << env
      env['PATH_INFO'] = '/changed'
      env['SCRIPT_NAME'] = '/prefix'
      env['QUERY_STRING'] = 'tampered=1'
      env['rack.request.parsed_state'] = true
      [404, {}, []]
    end
    second = lambda do |env|
      seen << env
      [200, {}, [env['PATH_INFO']]]
    end
    cascade = Rack::Cascade.new([first, second], [404], isolate: true)

    env = Rack::MockRequest.env_for("/foo?bar=2")
    status, _, body = cascade.call(env)
    status.must_equal 200
    body.each.to_a.must_equal ['/foo']

    # Each app got its own environment instance.
    seen[0].wont_be_same_as seen[1]
    # Business keys were restored before the next app was called.
    seen[1]['PATH_INFO'].must_equal '/foo'
    seen[1]['SCRIPT_NAME'].must_equal ''
    seen[1]['QUERY_STRING'].must_equal 'bar=2'
    seen[1].wont_include 'rack.request.parsed_state'
    # The caller's environment is untouched.
    env['PATH_INFO'].must_equal '/foo'
    env.wont_include 'rack.request.parsed_state'
  end

  it "keeps server-owned objects shared when isolating" do
    shared = Object.new
    seen = []
    first = lambda { |env| seen << env; [404, {}, []] }
    second = lambda { |env| seen << env; [200, {}, ["ok"]] }
    cascade = Rack::Cascade.new([first, second], [404], isolate: true)

    env = Rack::MockRequest.env_for("/foo")
    env['rack.errors'] = shared
    env['rack.logger'] = shared
    env['rack.hijack'] = shared
    cascade.call(env)

    Rack::Cascade::SHARED_ENV_KEYS.each do |key|
      next unless seen[0].key?(key)
      seen[0][key].must_be_same_as seen[1][key]
      seen[0][key].must_be_same_as env[key]
    end
  end

  it "rewinds rewindable input for later apps when isolating" do
    consumed = []
    reader = lambda do |env|
      consumed << env['rack.input'].read
      [404, {}, []]
    end
    cascade = Rack::Cascade.new([reader, reader], [404], isolate: true)

    env = Rack::MockRequest.env_for("/", input: "payload")
    cascade.call(env)

    # The second app reads the full body again instead of getting "".
    consumed.must_equal ['payload', 'payload']
  end

  it "raises instead of replaying a consumed non-rewindable input without replay mode" do
    non_rewindable = StringIO.new("payload".dup)
    non_rewindable.instance_eval { undef :rewind }

    reader = lambda { |env| env['rack.input'].read; [404, {}, []] }
    cascade = Rack::Cascade.new([reader, reader], [404], isolate: true)
    env = Rack::MockRequest.env_for("/")
    env['rack.input'] = non_rewindable

    proc { cascade.call(env) }.must_raise(Rack::Cascade::InputReplayError)
  end

  it "never hands an empty body to the second app for a consumed non-rewindable input" do
    non_rewindable = StringIO.new("payload".dup)
    non_rewindable.instance_eval { undef :rewind }

    bodies = []
    reader = lambda do |env|
      bodies << env['rack.input'].read
      [404, {}, []]
    end
    cascade = Rack::Cascade.new([reader, reader], [404], isolate: true)
    env = Rack::MockRequest.env_for("/")
    env['rack.input'] = non_rewindable

    proc { cascade.call(env) }.must_raise(Rack::Cascade::InputReplayError)
    # Only the first app ran; the second app must not silently see "".
    bodies.must_equal ['payload']
  end

  it "stages non-rewindable input into a bounded replay buffer" do
    non_rewindable = StringIO.new("payload".dup)
    non_rewindable.instance_eval { undef :rewind }

    consumed = []
    reader = lambda do |env|
      consumed << env['rack.input'].read
      [404, {}, []]
    end
    cascade = Rack::Cascade.new([reader, reader], [404], replay_input: true)
    env = Rack::MockRequest.env_for("/")
    env['rack.input'] = non_rewindable

    status, _, _ = cascade.call(env)
    status.must_equal 404
    consumed.must_equal ['payload', 'payload']
    # The staged buffer is rewindable and the original input is left open.
    non_rewindable.wont_be :closed?
  end

  it "raises InputReplayError when the staged input exceeds the buffer limit" do
    non_rewindable = StringIO.new("x" * 10)
    non_rewindable.instance_eval { undef :rewind }

    noop = lambda { |env| [404, {}, []] }
    cascade = Rack::Cascade.new([noop, noop], [404], replay_input: true,
                                                   input_buffer_limit: 4)
    env = Rack::MockRequest.env_for("/")
    env['rack.input'] = non_rewindable

    error = proc { cascade.call(env) }.must_raise(Rack::Cascade::InputReplayError)
    error.message.must_include '4'
  end

  it "accepts an unbounded buffer when input_buffer_limit is nil" do
    non_rewindable = StringIO.new("x" * 100)
    non_rewindable.instance_eval { undef :rewind }

    consumed = []
    reader = lambda do |env|
      consumed << env['rack.input'].read
      [404, {}, []]
    end
    cascade = Rack::Cascade.new([reader, reader], [404], replay_input: true,
                                                   input_buffer_limit: nil)
    env = Rack::MockRequest.env_for("/")
    env['rack.input'] = non_rewindable

    cascade.call(env)
    consumed.must_equal ['x' * 100, 'x' * 100]
  end

  it "rejects non-positive buffer limits at construction time" do
    proc { Rack::Cascade.new([], [404], input_buffer_limit: 0) }.
      must_raise(ArgumentError)
    proc { Rack::Cascade.new([], [404], input_buffer_limit: -1) }.
      must_raise(ArgumentError)
  end

  it "combines isolation and replay without leaking buffered input into the caller env" do
    non_rewindable = StringIO.new("payload".dup)
    non_rewindable.instance_eval { undef :rewind }

    seen_inputs = []
    reader = lambda do |env|
      seen_inputs << env['rack.input'].read
      [404, {}, []]
    end
    cascade = Rack::Cascade.new([reader, reader], [404], isolate: true,
                                                   replay_input: true)
    env = Rack::MockRequest.env_for("/")
    env['rack.input'] = non_rewindable

    cascade.call(env)
    seen_inputs.must_equal ['payload', 'payload']
    # The staging buffer is private to the cascade attempts.
    env['rack.input'].must_be_same_as non_rewindable
  end

  it "reports a body close failure separately from the cascade status" do
    failing_body = Object.new
    def failing_body.close
      raise "disk on fire"
    end

    first = lambda { |env| [404, {}, failing_body] }
    second_called = false
    second = lambda { |env| second_called = true; [200, {}, ["ok"]] }
    cascade = Rack::Cascade.new([first, second], [404])

    error = proc { cascade.call(Rack::MockRequest.env_for("/")) }.
      must_raise(Rack::Cascade::BodyCloseError)
    error.message.must_include "disk on fire"
    # The close failure is not treated as a route miss: the next app is not tried.
    second_called.must_equal false
  end

  it "still closes every replaced body when isolating" do
    bodies = Array.new(2) { StringIO.new }
    apps = bodies.map do |body|
      lambda { |env| [404, {}, body] }
    end
    apps << lambda { |env| [200, {}, ["ok"]] }
    cascade = Rack::Cascade.new(apps, [404], isolate: true)

    status, _, _ = cascade.call(Rack::MockRequest.env_for("/"))
    status.must_equal 200
    bodies.each { |body| body.must_be :closed? }
  end

  it "does not close the final response body" do
    final_body = StringIO.new("final")
    app = lambda { |env| [200, {}, final_body] }
    cascade = Rack::Cascade.new([app])

    cascade.call(Rack::MockRequest.env_for("/"))
    final_body.wont_be :closed?
  end
end
