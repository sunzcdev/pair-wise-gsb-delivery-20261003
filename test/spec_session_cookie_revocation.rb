# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require_relative 'helper'
require 'rack/response'
require 'rack/lint'
require 'rack/mock'

require_relative '../lib/rack/session/cookie'

describe Rack::Session::Cookie do
  reader = lambda do |env|
    session = env["rack.session"]
    # Read the session repeatedly within the same request; the epoch
    # callback must still only run once per subject.
    3.times { session["user_id"] }
    Rack::Response.new(session["user_id"].inspect).to_a
  end

  incrementor = lambda do |env|
    env["rack.session"]["counter"] ||= 0
    env["rack.session"]["counter"] += 1
    Rack::Response.new(env["rack.session"]["counter"].to_s).to_a
  end

  login = lambda do |env|
    env["rack.session"]["user_id"] = "alice"
    Rack::Response.new("logged in").to_a
  end

  switch_subject = lambda do |env|
    env["rack.session"]["user_id"] = "bob"
    Rack::Response.new("switched").to_a
  end

  logout = lambda do |env|
    env["rack.session"].delete("user_id")
    Rack::Response.new("logged out").to_a
  end

  renewer = lambda do |env|
    env["rack.session"]["counter"] = 1
    env["rack.session.options"][:renew] = true
    Rack::Response.new("renewed").to_a
  end

  def response_for(options = {})
    request_options = options.fetch(:request, {})
    cookie = if options[:cookie].is_a?(Rack::Response)
      options[:cookie]["Set-Cookie"]
    else
      options[:cookie]
    end
    request_options["HTTP_COOKIE"] = cookie || ""

    app = Rack::Session::Cookie.new(*options[:app])
    app = Rack::Lint.new(app)
    Rack::MockRequest.new(app).get("/", request_options)
  end

  def app_with(endpoint)
    [endpoint, { secrets: @secret, revocation: @revocation }]
  end

  def decrypt_cookie(response)
    value = response["Set-Cookie"].split(";").first.split("=", 2).last
    encryptor = Rack::Session::Encryptor.new(@secret, purpose: nil)
    encryptor.decrypt(Rack::Utils.unescape(value))
  end

  before do
    @secret = SecureRandom.random_bytes(64)

    # In-memory epoch store, standing in for an application database. The
    # library itself has no database client; it only sees this callback.
    @epochs = {}
    @calls = []
    @fail = false

    epoch_callback = lambda do |subject|
      @calls << subject
      raise "epoch store unavailable" if @fail
      @epochs[subject]
    end

    @revocation = { subject: "user_id", epoch: epoch_callback }
  end

  it "never invokes the epoch callback for anonymous sessions" do
    response = response_for(app: app_with(incrementor))
    response.body.must_equal "1"
    @calls.must_equal []

    response = response_for(app: app_with(incrementor), cookie: response)
    response.body.must_equal "2"
    @calls.must_equal []

    decrypt_cookie(response).wont_include "session_epoch"
  end

  it "stamps the current epoch into an authenticated cookie" do
    @epochs["alice"] = 1

    response = response_for(app: app_with(login))
    @calls.must_equal ["alice"]

    session = decrypt_cookie(response)
    session["user_id"].must_equal "alice"
    session["session_epoch"].must_equal 1
  end

  it "accepts matching epochs and calls the callback exactly once per request" do
    @epochs["alice"] = 1

    response = response_for(app: app_with(login))
    @calls.must_equal ["alice"]

    response = response_for(app: app_with(reader), cookie: response)
    response.body.must_equal '"alice"'
    @calls.must_equal ["alice", "alice"]

    response = response_for(app: app_with(reader), cookie: response)
    response.body.must_equal '"alice"'
    @calls.must_equal ["alice", "alice", "alice"]
  end

  it "rejects the session once the epoch is bumped" do
    @epochs["alice"] = 1

    response = response_for(app: app_with(login))
    cookie = response["Set-Cookie"]
    @calls.must_equal ["alice"]

    # "Log out of all devices": the application bumps the epoch.
    @epochs["alice"] = 2

    response = response_for(app: app_with(reader), cookie: cookie)
    response.body.must_equal "nil"
    @calls.must_equal ["alice", "alice"]
  end

  it "cannot revive a revoked cookie through renewal" do
    @epochs["alice"] = 1

    response = response_for(app: app_with(login))
    old_cookie = response["Set-Cookie"]
    @calls.must_equal ["alice"]

    @epochs["alice"] = 2

    # The old cookie is rejected on read, so renewal only issues a fresh
    # anonymous cookie; the stale epoch is never re-stamped.
    response = response_for(app: app_with(renewer), cookie: old_cookie)
    @calls.must_equal ["alice", "alice"]

    renewed = decrypt_cookie(response)
    renewed.wont_include "user_id"
    renewed.wont_include "session_epoch"

    # The renewed cookie is anonymous: no further callback calls.
    response = response_for(app: app_with(reader), cookie: response)
    response.body.must_equal "nil"
    @calls.must_equal ["alice", "alice"]

    # And the original cookie stays rejected.
    response = response_for(app: app_with(reader), cookie: old_cookie)
    response.body.must_equal "nil"
    @calls.must_equal ["alice", "alice", "alice"]
  end

  it "resolves the epoch freshly when the subject changes" do
    @epochs["alice"] = 1
    @epochs["bob"] = 7

    response = response_for(app: app_with(login))
    @calls.must_equal ["alice"]

    # One request: read validates "alice", write resolves "bob".
    response = response_for(app: app_with(switch_subject), cookie: response)
    @calls.must_equal ["alice", "alice", "bob"]

    session = decrypt_cookie(response)
    session["user_id"].must_equal "bob"
    session["session_epoch"].must_equal 7

    # Bumping alice's epoch does not affect bob's cookie.
    @epochs["alice"] = 2
    response = response_for(app: app_with(reader), cookie: response)
    response.body.must_equal '"bob"'
    @calls.must_equal ["alice", "alice", "bob", "bob"]
  end

  it "drops the epoch when the session becomes anonymous" do
    @epochs["alice"] = 1

    response = response_for(app: app_with(login))
    @calls.must_equal ["alice"]

    response = response_for(app: app_with(logout), cookie: response)
    @calls.must_equal ["alice", "alice"]

    session = decrypt_cookie(response)
    session.wont_include "user_id"
    session.wont_include "session_epoch"
  end

  it "fails closed when the callback is unavailable on read" do
    @epochs["alice"] = 1

    response = response_for(app: app_with(login))
    cookie = response["Set-Cookie"]
    @calls.must_equal ["alice"]

    @fail = true

    response = response_for(app: app_with(reader), cookie: cookie)
    response.body.must_equal "nil"
    # The failure is memoized: repeated reads cost one callback call.
    @calls.must_equal ["alice", "alice"]
  end

  it "does not persist the session when the callback is unavailable on write" do
    @epochs["alice"] = 1
    @fail = true

    response = response_for(app: app_with(login))
    response["Set-Cookie"].must_be_nil
    @calls.must_equal ["alice"]
  end

  it "treats a nil epoch as unavailable" do
    # The callback returns nil for unknown subjects.
    response = response_for(app: app_with(login))
    response["Set-Cookie"].must_be_nil
    @calls.must_equal ["alice"]
  end

  it "requires a callable epoch callback" do
    lambda do
      Rack::Session::Cookie.new(reader, secrets: @secret, revocation: { subject: "user_id", epoch: 1 })
    end.must_raise ArgumentError
  end
end
