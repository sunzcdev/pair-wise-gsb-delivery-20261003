# encoding: utf-8
# frozen_string_literal: true
require 'rack/session'

RSpec.describe "session restoration with a flaky identity store" do
  # A fake identity store that can be switched between an "outage" phase and a
  # "recovered" phase, and from which identities can be deleted for good.
  class FakeIdentityStore
    def initialize
      @identities = {}
      @available = true
      @lookups = []
    end

    attr_reader :lookups

    def add_identity(id, user)
      @identities[id] = user
    end

    def remove_identity(id)
      @identities.delete(id)
    end

    def available!
      @available = true
    end

    def unavailable!
      @available = false
    end

    def find(id)
      @lookups << id
      raise Warden::IdentityStoreTemporarilyUnavailable, "identity store is down" unless @available
      @identities[id] # nil means the identity is genuinely gone
    end

    def reset!
      @identities = {}
      @available = true
      @lookups = []
    end
  end

  # Registered serializer blocks close over lexical constants, so the fake
  # store is a constant rather than an RSpec +let+ helper.
  IDENTITY_STORE = FakeIdentityStore.new
  let(:identity_store) { IDENTITY_STORE }

  before(:all) do
    load_strategies

    Warden::Strategies.add(:store_pass) do
      def authenticate!
        request.env['warden.spec.strategies'] ||= []
        request.env['warden.spec.strategies'] << :store_pass
        success!({ :id => 1, :name => "Alice" })
      end
    end

    Warden::Manager.serialize_into_session { |user| user[:id] }
    Warden::Manager.serialize_from_session { |id| IDENTITY_STORE.find(id) }
  end

  before(:each) do
    IDENTITY_STORE.reset!
  end

  after(:all) do
    Warden::Manager.serialize_into_session { |u| u }
    Warden::Manager.serialize_from_session { |k| k }
  end

  after(:each) do
    Warden::Manager._after_failed_fetch.clear
  end

  # Runs a request sharing the given rack session hash, so a new Warden::Proxy
  # is built for each call but the serialized session survives in between.
  def call_with_session(session, app, env_overrides = {})
    env = env_with_params("/", {}, { "rack.session" => session }.merge(env_overrides))
    setup_rack(app).call(env)
    env
  end

  let(:login_app) do
    lambda do |env|
      env['warden'].authenticate!(:store_pass)
      valid_response
    end
  end

  # Only consults the restored session; never runs authentication strategies.
  let(:user_app) do
    lambda do |env|
      if env['warden'].user
        [200, { "Content-Type" => "text/plain" }, ["OK"]]
      else
        [401, { "Content-Type" => "text/plain" }, ["unauthenticated"]]
      end
    end
  end

  let(:check_app) do
    lambda do |env|
      env['warden'].authenticate(:store_pass)
      if env['warden'].authenticated?
        [200, { "Content-Type" => "text/plain" }, ["OK"]]
      else
        [401, { "Content-Type" => "text/plain" }, ["unauthenticated"]]
      end
    end
  end

  it "keeps the raw session value during the outage phase" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)
    expect(session["warden.user.default.key"]).to eq(1)

    identity_store.unavailable!
    expect {
      call_with_session(session, check_app)
    }.not_to change { session["warden.user.default.key"] }
  end

  it "is not authenticated while the identity store is down but reports the reason" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    identity_store.unavailable!
    env = call_with_session(session, check_app)

    warden = env['warden']
    expect(warden).not_to be_authenticated
    expect(warden.session_restore_status).to eq(:unavailable)
    expect(warden).to be_identity_store_temporarily_unavailable
  end

  it "does not fall back to other authentication strategies while unavailable" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    identity_store.unavailable!
    session["warden.spec.strategies"] = []
    call_with_session(session, check_app)

    # :store_pass would have logged someone in; it must not run when we cannot
    # verify the existing session.
    expect(session["warden.spec.strategies"]).to eq([])
  end

  it "does not delete the session and lets the original session keep working after recovery" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    # Failure phase across several requests.
    identity_store.unavailable!
    3.times do
      env = call_with_session(session, check_app)
      expect(env['rack.session']["warden.user.default.key"]).to eq(1)
      expect(env['warden']).not_to be_authenticated
    end

    # Recovery phase: untouched session restores the very same identity.
    identity_store.available!
    env = call_with_session(session, check_app)
    expect(env['rack.session']["warden.user.default.key"]).to eq(1)
    expect(env['warden']).to be_authenticated
    expect(env['warden'].user[:name]).to eq("Alice")
  end

  it "allows an explicit re-restore within the same request once the identity store recovers" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    identity_store.unavailable!
    app = lambda do |env|
      env['warden'].user
      expect(env['warden']).to be_identity_store_temporarily_unavailable

      identity_store.available!
      user = env['warden'].restore_user_from_session!
      expect(user[:name]).to eq("Alice")
      expect(env['warden'].session_restore_status).to eq(:restored)
      valid_response
    end
    call_with_session(session, app)
    expect(session["warden.user.default.key"]).to eq(1)
  end

  it "hits the identity store only once per request until an explicit re-restore" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    identity_store.unavailable!
    app = lambda do |env|
      env['warden'].user
      env['warden'].user
      env['warden'].session_restore_status
      valid_response
    end
    call_with_session(session, app)
    failed_lookups = identity_store.lookups.dup

    identity_store.available!
    app2 = lambda do |env|
      env['warden'].user
      re_restored = env['warden'].restore_user_from_session!
      env['warden'].user
      expect(re_restored[:name]).to eq("Alice")
      valid_response
    end
    call_with_session(session, app2)

    expect(failed_lookups).to eq([1])
    # Second request: one cached lookup plus one more caused by the explicit
    # re-restore.
    expect(identity_store.lookups).to eq([1, 1, 1])
  end

  it "does not resurrect a genuinely deleted identity through retries" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    # The account is removed for real.
    identity_store.remove_identity(1)
    env = call_with_session(session, user_app)

    expect(env['rack.session']).not_to have_key("warden.user.default.key")
    expect(env['warden'].session_restore_status).to eq(:not_found)
    expect(env['warden']).not_to be_authenticated
    expect(env['warden']).not_to be_identity_store_temporarily_unavailable

    # Even an explicit retry cannot bring it back.
    expect(env['warden'].restore_user_from_session!).to be_nil
    expect(env['rack.session']).not_to have_key("warden.user.default.key")
  end

  it "treats an outage followed by a real deletion as not found on recovery" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    identity_store.unavailable!
    env = call_with_session(session, check_app)
    expect(env['rack.session']["warden.user.default.key"]).to eq(1)

    identity_store.remove_identity(1)
    identity_store.available!
    env = call_with_session(session, user_app)
    expect(env['rack.session']).not_to have_key("warden.user.default.key")
    expect(env['warden'].session_restore_status).to eq(:not_found)
  end

  it "marks the failure entry point with the temporarily unavailable reason" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    failure_app = lambda do |env|
      reason = env["warden.options"] && env["warden.options"][:reason]
      status = reason == Warden::Proxy::IDENTITY_STORE_TEMPORARILY_UNAVAILABLE ? 503 : 401
      [status, { "Content-Type" => "text/plain", "X-Warden-Reason" => reason.to_s }, ["fail"]]
    end

    app = lambda { |env| env['warden'].authenticate!(:store_pass); valid_response }
    builder = setup_rack(app, :failure_app => failure_app)

    identity_store.unavailable!
    status, headers, _body = builder.call(env_with_params("/", {}, { "rack.session" => session }))
    expect(status).to eq(503)
    expect(headers["X-Warden-Reason"]).to eq("identity_store_temporarily_unavailable")
    expect(session["warden.user.default.key"]).to eq(1)
  end

  it "tells after_failed_fetch hooks the identity store is unavailable (and not when deleted)" do
    identity_store.add_identity(1, { :id => 1, :name => "Alice" })
    session = {}
    call_with_session(session, login_app)

    # Register after login so only restoration attempts are observed.
    reasons = []
    Warden::Manager.after_failed_fetch do |_user, _auth, opts|
      reasons << opts[:reason]
    end

    identity_store.unavailable!
    call_with_session(session, user_app)
    expect(reasons).to eq([Warden::Proxy::IDENTITY_STORE_TEMPORARILY_UNAVAILABLE])

    identity_store.remove_identity(1)
    identity_store.available!
    call_with_session(session, user_app)
    expect(reasons).to eq([
      Warden::Proxy::IDENTITY_STORE_TEMPORARILY_UNAVAILABLE,
      nil
    ])
  end
end




