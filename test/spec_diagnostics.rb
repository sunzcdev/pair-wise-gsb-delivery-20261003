# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/diagnostics'
  require_relative '../lib/rack/builder'
  require_relative '../lib/rack/lint'
  require_relative '../lib/rack/mock_request'
  require_relative '../lib/rack/mock_response'
  require_relative '../lib/rack/runtime'
end

describe Rack::Diagnostics do
  # A middleware that turns a valid response into an invalid one by changing
  # the status after the inner application has returned.
  class BadStatusMiddleware
    def initialize(app)
      @app = app
    end

    def call(env)
      _status, headers, body = @app.call(env)
      [99, headers, body]
    end
  end

  # A middleware that corrupts the environment before calling the inner
  # application (a CGI key must hold a String value).
  class BadEnvMiddleware
    def initialize(app)
      @app = app
    end

    def call(env)
      env["SERVER_NAME"] = 12345
      @app.call(env)
    end
  end

  # A middleware that adds an invalid (upper case) response header after the
  # inner application returns.
  class BadHeaderMiddleware
    def initialize(app)
      @app = app
    end

    def call(env)
      status, headers, body = @app.call(env)
      headers["X-Bad"] = "1"
      [status, headers, body]
    end
  end

  # A middleware that wraps the body of the inner application in a new body
  # which still yields every inner chunk.
  class WrappingMiddleware
    def initialize(app)
      @app = app
    end

    def call(env)
      status, headers, body = @app.call(env)
      new_body = Object.new
      new_body.define_singleton_method(:each) do |&block|
        body.each { |chunk| block.call(chunk) }
      end
      [status, headers, new_body]
    end
  end

  # An enumerable body that yields a non-string value during iteration.
  class LazyBadBody
    def each(&block)
      block.call "good"
      block.call 42
    end
  end

  # A streaming body that records each invocation on a shared counter array.
  class StreamingBody
    def initialize(counter)
      @counter = counter
    end

    def call(stream)
      @counter << (@counter.size + 1)
      stream.write("hi")
    end
  end

  def env_for(uri = "/", options = {})
    Rack::MockRequest.env_for(uri, options)
  end

  def diagnostic_app(&block)
    Rack::Builder.new(diagnose: true, &block).to_app
  end

  valid_endpoint = lambda do |env|
    [200, {"content-type" => "text/plain", "content-length" => "2"}, ["ok"]]
  end

  it "is not enabled by default and leaves the builder stack untouched" do
    builder = Rack::Builder.new do
      use Rack::Runtime
      run valid_endpoint
    end

    app = builder.to_app
    app.wont_be_kind_of Rack::Diagnostics::Boundary

    response = Rack::MockRequest.new(app).get("/")
    response.status.must_equal 200
    response.body.must_equal "ok"
  end

  it "does not add diagnostics keys to the environment when disabled" do
    observed = nil
    inspector = Class.new do
      define_method(:initialize) { |app| @app = app }
      define_method(:call) do |env|
        observed = env
        @app.call(env)
      end
    end

    app = Rack::Builder.new do
      use inspector
      run valid_endpoint
    end.to_app

    Rack::MockRequest.new(app).get("/")
    observed.keys.wont_include "rack.diagnostics.frames"
    observed.keys.wont_include "rack.diagnostics.last_response"
  end

  it "can be enabled with the diagnose: option" do
    app = diagnostic_app do
      run valid_endpoint
    end

    app.must_be_kind_of Rack::Diagnostics::Boundary
    Rack::MockRequest.new(app).get("/").status.must_equal 200
  end

  it "can be enabled with the diagnose DSL method" do
    app = Rack::Builder.new do
      diagnose
      run valid_endpoint
    end.to_app

    app.must_be_kind_of Rack::Diagnostics::Boundary
  end

  it "wraps each middleware and the endpoint in a boundary" do
    outer_boundary = diagnostic_app do
      use Rack::Runtime
      use BadStatusMiddleware
      run valid_endpoint
    end

    # The outermost object is the boundary validating the Runtime middleware.
    outer_boundary.must_be_kind_of Rack::Diagnostics::Boundary
    outer_boundary.label.must_equal "Rack::Runtime instance"

    # That boundary wraps the Runtime middleware, whose app is the boundary
    # validating the BadStatus middleware, and so on down to the endpoint.
    runtime = outer_boundary.instance_variable_get(:@app)
    runtime.must_be_kind_of Rack::Runtime

    bad_status_boundary = runtime.instance_variable_get(:@app)
    bad_status_boundary.must_be_kind_of Rack::Diagnostics::Boundary
    bad_status_boundary.label.must_equal "BadStatusMiddleware instance"

    bad_status = bad_status_boundary.instance_variable_get(:@app)
    bad_status.must_be_kind_of BadStatusMiddleware

    endpoint_boundary = bad_status.instance_variable_get(:@app)
    endpoint_boundary.must_be_kind_of Rack::Diagnostics::Boundary
    endpoint_boundary.label.must_match(/Proc/)
  end

  it "passes a fully valid stack through unchanged" do
    app = diagnostic_app do
      use Rack::Runtime
      run valid_endpoint
    end

    response = Rack::MockRequest.new(app).get("/")
    response.status.must_equal 200
    response.body.must_equal "ok"
    response.headers["x-runtime"].wont_be_nil
  end

  it "raises a ContractViolation which is also a LintError" do
    app = diagnostic_app do
      run lambda { |env| [99, {"content-type" => "text/plain"}, ["x"]] }
    end

    proc { Rack::MockRequest.new(app).get("/") }.must_raise(Rack::Diagnostics::ContractViolation).
      must_be_kind_of(Rack::Lint::LintError)
  end

  it "attributes a response violation to the endpoint and attaches its request summary" do
    app = diagnostic_app do
      use Rack::Runtime
      run lambda { |env| [99, {"content-type" => "text/plain"}, ["x"]] }
    end

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      Rack::MockRequest.new(app).get("/")
    end

    error.phase.must_equal :response
    error.component.must_match(/Proc/)
    error.previous_summary.must_be_kind_of Rack::Diagnostics::RequestSummary
    error.message.must_include "Status must be an Integer"
  end

  it "attributes a response violation to the named middleware that produced it" do
    app = diagnostic_app do
      use BadStatusMiddleware
      run valid_endpoint
    end

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      Rack::MockRequest.new(app).get("/")
    end

    error.component.must_equal "BadStatusMiddleware instance"
    error.phase.must_equal :response
  end

  it "includes the inner boundary's response summary as the previous boundary" do
    app = diagnostic_app do
      use BadHeaderMiddleware
      use Rack::Runtime
      run valid_endpoint
    end

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      Rack::MockRequest.new(app).get("/")
    end

    # The invalid header is observed when the outer (bad header) boundary
    # validates the response returned by the inner Runtime boundary.
    error.component.must_equal "BadHeaderMiddleware instance"
    error.phase.must_equal :response
    error.previous_summary.must_be_kind_of Rack::Diagnostics::ResponseSummary
    error.previous_summary.component.must_equal "Rack::Runtime instance"
    error.previous_summary.status.must_equal 200
  end

  it "attributes an environment violation to the component that received it" do
    app = diagnostic_app do
      use BadEnvMiddleware
      use Rack::Runtime
      run valid_endpoint
    end

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      Rack::MockRequest.new(app).get("/")
    end

    error.phase.must_equal :request
    # The Runtime boundary is the first boundary that validates the
    # environment already corrupted by the middleware above it.
    error.component.must_equal "Rack::Runtime instance"
    error.previous_summary.must_be_kind_of Rack::Diagnostics::RequestSummary
    error.previous_summary.component.must_equal "BadEnvMiddleware instance"
  end

  it "reports a non-Hash environment as a request phase violation" do
    app = diagnostic_app do
      run valid_endpoint
    end

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      app.call(5)
    end

    error.phase.must_equal :request
    error.message.must_include "not a Hash"
  end

  it "does not iterate an enumerable body while building the response" do
    iterated = false
    lazy_body = Object.new
    lazy_body.define_singleton_method(:each) do |&block|
      iterated = true
      block.call "ok"
    end

    app = diagnostic_app do
      run lambda { |env| [200, {"content-type" => "text/plain", "content-length" => "2"}, lazy_body] }
    end

    response = app.call(env_for)
    iterated.must_equal false

    response[2].each { |chunk| chunk.must_equal "ok" }
    iterated.must_equal true
    response[2].close
  end

  it "attributes a lazily discovered body violation to the component that created the wrapper" do
    app = diagnostic_app do
      run lambda { |env| [200, {"content-type" => "text/plain"}, LazyBadBody.new] }
    end

    response = app.call(env_for)
    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      response[2].each { |_| }
    end

    error.phase.must_equal :body
    error.component.must_match(/Proc/)
    error.message.must_include "Body yielded non-string value"
    response[2].close rescue nil
  end

  it "keeps body attribution when an outer middleware replaces the body" do
    app = diagnostic_app do
      use WrappingMiddleware
      run lambda { |env| [200, {"content-type" => "text/plain"}, LazyBadBody.new] }
    end

    response = app.call(env_for)
    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      response[2].each { |_| }
    end

    # The failure belongs to the endpoint boundary that wrapped LazyBadBody,
    # even though the outer middleware's body is doing the iterating.
    error.phase.must_equal :body
    error.component.must_match(/Proc/)
    response[2].close rescue nil
  end

  it "does not invoke a streaming body while building the response" do
    counter = []
    app = diagnostic_app do
      run lambda { |env| [200, {"content-type" => "text/plain"}, StreamingBody.new(counter)] }
    end

    response = app.call(env_for)
    # The streaming body is not touched while the response is built.
    counter.must_equal []

    mock = Rack::MockResponse.new(*response)
    # MockResponse cannot buffer a call-only body up front; consuming the
    # response body is what lazily invokes the streaming body.
    mock.body.must_equal "hi"
    counter.size.must_equal 1
  end

  it "invokes each component exactly once" do
    calls = 0
    counting_middleware = Class.new do
      define_method(:initialize) { |app| @app = app }
      define_method(:call) do |env|
        calls += 1
        @app.call(env)
      end
    end

    app = diagnostic_app do
      use counting_middleware
      run lambda { |env| calls += 1; valid_endpoint.call(env) }
    end

    Rack::MockRequest.new(app).get("/")
    calls.must_equal 2
  end

  it "never writes header values or body contents into a report" do
    secret_endpoint = lambda do |env|
      [99, {"authorization" => "Bearer super-secret-token", "content-type" => "text/plain"},
       ["super-secret-body"]]
    end

    app = diagnostic_app do
      run secret_endpoint
    end

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      Rack::MockRequest.new(app).get("/", "HTTP_AUTHORIZATION" => "Bearer request-secret")
    end

    message = error.message
    message.wont_include "super-secret-token"
    message.wont_include "request-secret"
    message.wont_include "super-secret-body"
    # Only structural header *names* are shown (here the request-side CGI
    # name), never their values.
    message.must_include "HTTP_AUTHORIZATION"
  end

  it "records body chunk and byte counts in the response summary" do
    app = diagnostic_app do
      run lambda { |env| [200, {"content-type" => "text/plain", "content-length" => "5"}, ["he", "llo"]] }
    end

    response = app.call(env_for)
    summary = response[2].instance_variable_get(:@response_summary)
    summary.chunks.must_equal 0
    summary.bytes.must_equal 0

    response[2].each { |_| }
    summary.chunks.must_equal 2
    summary.bytes.must_equal 5
    response[2].close
  end

  it "works with map blocks" do
    app = Rack::Builder.new(diagnose: true) do
      map "/sub" do
        run lambda { |env| [99, {"content-type" => "text/plain"}, ["x"]] }
      end
      run valid_endpoint
    end.to_app

    assert_raises(Rack::Diagnostics::ContractViolation) do
      Rack::MockRequest.new(app).get("/sub")
    end

    Rack::MockRequest.new(app).get("/").status.must_equal 200
  end

  it "composes with an explicitly stacked Rack::Lint" do
    inner = Rack::Lint.new(valid_endpoint)
    app = diagnostic_app do
      run inner
    end

    Rack::MockRequest.new(app).get("/").body.must_equal "ok"
  end

  it "keeps working when the builder freezes the app and middleware" do
    app = Rack::Builder.new(diagnose: true) do
      freeze_app
      use Rack::Runtime
      run valid_endpoint
    end.to_app

    # Two requests prove per-request state does not live on the frozen
    # boundary objects.
    2.times do
      response = Rack::MockRequest.new(app).get("/")
      response.status.must_equal 200
      response.body.must_equal "ok"
    end
  end

  it "can be enabled through Builder.new_from_string" do
    app = Rack::Builder.new_from_string(
      "run lambda { |env| [200, {'content-type' => 'text/plain', 'content-length' => '2'}, ['ok']] }",
      "(diagnostics-test)",
      diagnose: true
    )

    app.must_be_kind_of Rack::Diagnostics::Boundary
    Rack::MockRequest.new(app).get("/").body.must_equal "ok"
  end

  it "cleans up its per-request frame stack after each request" do
    frame_depths = []
    inspector = Class.new do
      define_method(:initialize) { |app| @app = app }
      define_method(:call) do |env|
        frame_depths << env["rack.diagnostics.frames"]&.size
        @app.call(env)
      end
    end

    app = diagnostic_app do
      use inspector
      run valid_endpoint
    end

    3.times { Rack::MockRequest.new(app).get("/") }
    frame_depths.must_equal [1, 1, 1]
  end

  it "provides stable component labels" do
    Rack::Diagnostics.label_for(Class.new.new).must_match(/instance/)

    proc_label = Rack::Diagnostics.label_for(proc { })
    proc_label.must_match(/Proc/)

    object = Object.new
    Rack::Diagnostics.label_for(object).must_include "Object"
  end

  it "does not re-wrap an existing ContractViolation" do
    inner_error = Rack::Diagnostics::ContractViolation.new(
      "Inner instance", :response,
      Rack::Lint::LintError.new("original"), nil
    )

    boundary = Rack::Diagnostics::Boundary.new(
      lambda { |env| raise inner_error },
      "Outer instance"
    )

    error = assert_raises(Rack::Diagnostics::ContractViolation) do
      boundary.call(env_for)
    end

    error.must_be_same_as inner_error
    error.component.must_equal "Inner instance"
  end
end
