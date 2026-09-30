# encoding: utf-8
# frozen_string_literal: true

RSpec.describe "authentication evidence (step up / reauthentication)" do
  let(:clock) { Warden::Spec::Helpers::FakeClock.new(1000) }

  def call_app(&block)
    app = lambda do |env|
      block.call(env)
      valid_response
    end
    # Simulate a new request: a fresh Warden::Proxy is created per request,
    # while the rack session hash carries data across requests.
    @env.delete("warden")
    @env['rack.session'] ||= {}
    @thrown = catch(:warden) do
      setup_rack(app).call(@env)
    end
    @thrown.is_a?(Array) ? @thrown : nil
  end

  attr_reader :thrown

  before(:each) do
    load_strategies
    @env = env_with_params
    @env['rack.session'] = {}
  end

  describe "recording evidence" do
    it "records the winning strategy and completion time per scope" do
      recorded_at = nil
      call_app do |env|
        env['warden'].authenticate(:pass, :scope => :admin, :clock => clock)
        recorded_at = clock.now
      end

      raw = @env['rack.session'][Warden::AuthEvidence::SESSION_KEY]
      expect(raw).to eq("admin" => { "method" => "pass", "at" => recorded_at.to_f })
    end

    it "keeps evidence separate for different scopes" do
      call_app do |env|
        env['warden'].authenticate(:pass, :scope => :admin, :clock => clock)
        env['warden'].authenticate(:pass, :scope => :user, :clock => clock)
      end

      raw = @env['rack.session'][Warden::AuthEvidence::SESSION_KEY]
      expect(raw.keys).to match_array(%w[admin user])
    end

    it "exposes method and completion time through the proxy" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
        expect(env['warden'].authentication_method).to eq(:pass)
        expect(env['warden'].last_authentication_at).to eq(Time.at(1000))
      end
    end
  end

  describe "restoring a user does not refresh the evidence" do
    it "keeps the original completion time across requests" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end

      clock.advance(60)

      call_app do |env|
        expect(env['warden'].user).to eq("Valid User")
        expect(env['warden'].last_authentication_at).to eq(Time.at(1000))
        expect(env['warden'].authentication_method).to eq(:pass)
      end

      raw = @env['rack.session'][Warden::AuthEvidence::SESSION_KEY]
      expect(raw["default"]["at"]).to eq(1000.0)
    end

    it "does not refresh when user is read more than once in a request" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end

      clock.advance(30)

      call_app do |env|
        2.times { env['warden'].user }
        env['warden'].authenticated?
      end

      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]["at"]).to eq(1000.0)
    end
  end

  describe "maximum authentication age" do
    before do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end
    end

    it "passes inside the allowed window" do
      clock.advance(300)

      call_app do |env|
        expect(env['warden'].reauthenticated?(:max_age => 300, :clock => clock)).to eq(true)
        expect(env['warden'].authenticated?(:max_age => 300, :clock => clock)).to eq(true)
      end
    end

    it "accepts the exact boundary moment" do
      clock.advance(300)

      call_app do |env|
        expect(env['warden'].reauthenticated?(:max_age => 300, :clock => clock)).to eq(true)
      end
    end

    it "fails one second past the allowed window" do
      clock.advance(301)

      call_app do |env|
        expect(env['warden'].reauthenticated?(:max_age => 300, :clock => clock)).to eq(false)
        # The plain login remains usable.
        expect(env['warden'].authenticated?).to eq(true)
      end
    end
  end

  describe "required method" do
    before do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end
    end

    it "passes when the recorded method is allowed" do
      call_app do |env|
        expect(env['warden'].reauthenticated?(:methods => :pass, :clock => clock)).to eq(true)
        expect(env['warden'].reauthenticated?(:methods => [:pass, :otp], :clock => clock)).to eq(true)
      end
    end

    it "fails when only a weak method was used" do
      call_app do |env|
        expect(env['warden'].reauthenticated?(:methods => :otp, :clock => clock)).to eq(false)
        expect(env['warden'].reauthenticated?(:method => :otp, :clock => clock)).to eq(false)
        expect(env['warden'].authenticated?(:scope => :default)).to eq(true)
      end
    end
  end

  describe "sensitive entry point" do
    it "throws :warden when the evidence is stale but keeps the login" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end

      clock.advance(301)

      result = call_app do |env|
        # :password needs credentials; with none supplied it does not re-run,
        # so the stale evidence cannot be refreshed.
        env['warden'].authenticate_recently!(:password, :max_age => 300,
                                             :clock => clock, :scope => :default)
      end

      # The throw is caught by the manager and rendered via the failure app.
      expect(result[0]).to eq(401)

      # The normal login is still present in the session.
      call_app do |env|
        expect(env['warden'].authenticated?).to eq(true)
        expect(env['warden'].reauthenticated?(:max_age => 300, :clock => clock)).to eq(false)
      end
    end

    it "throws when a strong method is required but only a weak one ran" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end

      result = call_app do |env|
        env['warden'].authenticate_recently!(:methods => :otp, :clock => clock, :scope => :default)
      end

      expect(result[0]).to eq(401)

      call_app do |env|
        expect(env['warden'].authenticated?).to eq(true)
        expect(env['warden'].authentication_method).to eq(:pass)
      end
    end

    it "allows the request when a fresh authentication is performed in the same request" do
      # A stale session (no evidence) exists.
      session = { "warden.user.default.key" => "Valid User" }
      @env['rack.session'] = session

      call_app do |env|
        expect(env['warden'].reauthenticated?(:max_age => 300, :clock => clock)).to eq(false)
      end

      # A later request supplies valid credentials; :password succeeds and
      # establishes fresh evidence.
      @env = env_with_params("/", "username" => "fred", "password" => "sekrit")
      @env['rack.session'] = session

      call_app do |env|
        user = env['warden'].authenticate_recently!(:password, :max_age => 300, :clock => clock)
        expect(user).to eq("Authenticated User")
        expect(env['warden'].authentication_method).to eq(:password)
      end

      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]).to eq(
        "method" => "password", "at" => 1000.0
      )
    end

    it "does not let a failing strategy refresh the evidence" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
      end

      clock.advance(10)

      call_app do |env|
        # :fail_with_user fails, so it must not refresh the evidence.
        env['warden'].authenticate_recently!(:fail_with_user, :methods => :otp,
                                            :clock => clock, :scope => :default)
      end

      expect(thrown[0]).to eq(401)
      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]["at"]).to eq(1000.0)
    end
  end

  describe "same-request caching" do
    it "applies the same check to cached hits during a request" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)

        # First call authenticates; a second call with a stricter requirement
        # must not be granted just because the strategy is cached.
        expect(env['warden'].reauthenticated?(:methods => :otp, :clock => clock)).to eq(false)
      end
    end

    it "does not refresh evidence on repeated reads in the authenticating request" do
      call_app do |env|
        env['warden'].authenticate(:pass, :clock => clock)
        clock.advance(50)
        env['warden'].user
        env['warden'].authenticated?
      end

      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]["at"]).to eq(1000.0)
    end
  end

  describe "logout" do
    it "clears evidence for the logged out scope only" do
      call_app do |env|
        env['warden'].authenticate(:pass, :scope => :admin, :clock => clock)
        env['warden'].authenticate(:pass, :scope => :user, :clock => clock)
        env['warden'].logout(:admin)
      end

      raw = @env['rack.session'][Warden::AuthEvidence::SESSION_KEY]
      expect(raw).to eq("user" => { "method" => "pass", "at" => 1000.0 })
    end

    it "clears all evidence on a general logout" do
      call_app do |env|
        env['warden'].authenticate(:pass, :scope => :admin, :clock => clock)
        env['warden'].authenticate(:pass, :scope => :user, :clock => clock)
        env['warden'].logout
      end

      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]).to be_nil
    end
  end

  describe "clock configuration" do
    it "uses the configured clock when no per-call clock is given" do
      app = lambda do |env|
        env['warden'].authenticate(:pass)
        valid_response
      end
      rack_app = setup_rack(app, :clock => clock)
      rack_app.call(@env)

      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]["at"]).to eq(1000.0)
    end

    it "supports a per-call clock overriding the configuration" do
      app = lambda do |env|
        env['warden'].authenticate(:pass, :clock => Warden::Spec::Helpers::FakeClock.new(5000))
        valid_response
      end
      rack_app = setup_rack(app, :clock => clock)
      rack_app.call(@env)

      expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]["at"]).to eq(5000.0)
    end
  end
end

