# frozen_string_literal: true

# Released under the MIT License.

require_relative 'helper'
require 'rack/response'
require 'rack/lint'
require 'rack/mock'

require_relative '../lib/rack/session/cookie'

describe Rack::Session::Cookie do
  SUBJECT_KEY = Rack::Session::Cookie::REVOCATION_SUBJECT_KEY
  EPOCH_KEY = Rack::Session::Cookie::REVOCATION_EPOCH_KEY

  # In-memory epoch store standing in for an application's database. The
  # library itself ships no database client; it only calls this proc.
  def make_epoch_store(initial = {})
    epochs = initial.dup
    calls = Hash.new(0)
    check = lambda do |subject|
      calls[subject] += 1
      epochs.fetch(subject)
    end
    [epochs, calls, check]
  end

  def response_for(app:, cookie: nil, check: @check)
    stack = Rack::Session::Cookie.new(app, secrets: @secret, revocation_check: check)
    stack = Rack::Lint.new(stack)
    request_options = {}
    request_options["HTTP_COOKIE"] = cookie if cookie
    Rack::MockRequest.new(stack).get("/", request_options)
  end

  def session_cookie(response)
    response["Set-Cookie"].split(';', 2).first
  end

  def login_app(subject)
    lambda do |env|
      env["rack.session"][SUBJECT_KEY] = subject
      env["rack.session"]["user"] = subject
      Rack::Response.new("logged in").to_a
    end
  end

  # Reads the session repeatedly within a single request.
  reader = lambda do |env|
    session = env["rack.session"]
    3.times { session["user"] }
    session.keys
    session.each { |*| }
    Rack::Response.new(session.to_hash.inspect).to_a
  end

  renewer = lambda do |env|
    env["rack.session.options"][:renew] = true
    env["rack.session"][SUBJECT_KEY] = "alice"
    env["rack.session"]["user"] = "alice"
    Rack::Response.new("renewed").to_a
  end

  switcher = lambda do |env|
    env["rack.session"][SUBJECT_KEY] = "bob"
    env["rack.session"]["user"] = "bob"
    Rack::Response.new("switched").to_a
  end

  anonymous = lambda do |env|
    env["rack.session"]["cart"] = "beans"
    Rack::Response.new(env["rack.session"]["cart"].to_s).to_a
  end

  before do
    @secret = SecureRandom.random_bytes(64)
    @epochs, @calls, @check = make_epoch_store("alice" => 1, "bob" => 5)
  end

  it "accepts the session when the cookie epoch matches the current epoch" do
    response = response_for(app: login_app("alice"))
    cookie = session_cookie(response)
    @calls["alice"].must_equal 1

    response = response_for(app: reader, cookie: cookie)
    response.body.must_include '"user" => "alice"'
    @calls["alice"].must_equal 2
  end

  it "rejects the session after the subject's epoch is bumped" do
    cookie = session_cookie(response_for(app: login_app("alice")))

    # "Log out of all devices": bump the epoch.
    @epochs["alice"] = 2

    response = response_for(app: reader, cookie: cookie)
    response.body.wont_include "alice"
    @calls["alice"].must_equal 2
  end

  it "never calls the revocation check for anonymous sessions" do
    response = response_for(app: anonymous)
    cookie = session_cookie(response)
    @calls.values.sum.must_equal 0

    response = response_for(app: anonymous, cookie: cookie)
    response.body.must_equal "beans"
    @calls.values.sum.must_equal 0
  end

  it "calls the revocation check at most once per subject per request" do
    cookie = session_cookie(response_for(app: login_app("alice")))
    @calls["alice"].must_equal 1

    # The reader app performs many session reads in one request.
    response_for(app: reader, cookie: cookie)
    @calls["alice"].must_equal 2

    response_for(app: reader, cookie: cookie)
    @calls["alice"].must_equal 3
  end

  it "checks the new subject when the subject switches during a request" do
    cookie = session_cookie(response_for(app: login_app("alice")))
    @calls["alice"].must_equal 1

    # Load checks the old subject (alice), write checks the new one (bob).
    response = response_for(app: switcher, cookie: cookie)
    @calls["alice"].must_equal 2
    @calls["bob"].must_equal 1

    # The new cookie is stamped with bob's current epoch and stays valid.
    response = response_for(app: reader, cookie: session_cookie(response))
    response.body.must_include '"user" => "bob"'
    @calls["bob"].must_equal 2
  end

  it "rejects the session when the revocation check raises" do
    cookie = session_cookie(response_for(app: login_app("alice")))

    failing = lambda do |subject|
      @calls[subject] += 1
      raise "database unavailable"
    end

    response = response_for(app: reader, cookie: cookie, check: failing)
    response.body.wont_include "alice"
    @calls["alice"].must_equal 2
  end

  it "rejects the session when the revocation check returns nil" do
    cookie = session_cookie(response_for(app: login_app("alice")))

    calls = Hash.new(0)
    unavailable = lambda do |subject|
      calls[subject] += 1
      nil
    end

    response = response_for(app: reader, cookie: cookie, check: unavailable)
    response.body.wont_include "alice"
    calls["alice"].must_equal 1
  end

  it "does not save the session when the revocation check is unavailable on write" do
    failing = ->(subject) { raise "database unavailable" }

    response = response_for(app: login_app("alice"), check: failing)
    response["Set-Cookie"].must_be_nil
  end

  it "cannot renew an old cookie back into validity after an epoch bump" do
    old_cookie = session_cookie(response_for(app: login_app("alice")))

    @epochs["alice"] = 2

    # Renewal re-stamps the epoch from the revocation check, not the cookie.
    response = response_for(app: renewer, cookie: old_cookie)
    renewed_cookie = session_cookie(response)

    # The renewed cookie carries the current epoch and is accepted.
    response = response_for(app: reader, cookie: renewed_cookie)
    response.body.must_include '"user" => "alice"'

    # The pre-bump cookie remains rejected.
    response = response_for(app: reader, cookie: old_cookie)
    response.body.wont_include "alice"
  end

  it "stamps the renewed cookie with the current epoch, not the cookie's epoch" do
    old_cookie = session_cookie(response_for(app: login_app("alice")))

    @epochs["alice"] = 2
    renewed_cookie = session_cookie(response_for(app: renewer, cookie: old_cookie))

    # Bump again: only a cookie stamped with epoch 2 survives until this
    # point, proving the renewal picked up epoch 2 rather than epoch 1.
    @epochs["alice"] = 3
    response = response_for(app: reader, cookie: renewed_cookie)
    response.body.wont_include "alice"
  end
end
