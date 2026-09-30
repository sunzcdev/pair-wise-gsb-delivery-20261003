# encoding: utf-8
# frozen_string_literal: true

Warden::Strategies.add(:strong_auth) do
  def authenticate!
    params["strong"] == "yes" ? success!("Strong User") : fail!("Strong authentication failed")
  end
end

RSpec.describe "fresh authentication" do
  before(:all) do
    load_strategies
  end

  before(:each) do
    @now = Time.at(1_000_000)
    @clock = proc { @now }
  end

  def build_app(logic, opts = {})
    app = lambda do |env|
      logic.call(env)
      valid_response
    end
    setup_rack(app, { :clock => @clock }.merge(opts))
  end

  def login_params
    { :username => "fred", :password => "sekrit" }
  end

  describe "authentication evidence" do
    it "records the method and completion time when authentication succeeds" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        evidence = env['warden'].authentication_evidence
        expect(evidence[:method]).to eq("password")
        expect(evidence[:authenticated_at]).to eq(Time.at(1_000_000))
      end).call(env_with_params("/", login_params))
    end

    it "stores the evidence in the session scoped by scope" do
      session = {}
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
      end).call(env_with_params("/", login_params, 'rack.session' => session))

      expect(session["warden.user.default.authentication"]).to eq(
        'method' => "password", 'authenticated_at' => 1_000_000.0
      )
    end

    it "does not record evidence for a manually set user" do
      build_app(lambda do |env|
        env['warden'].set_user("A User")
        expect(env['warden'].authentication_evidence).to be_nil
        expect(env['warden'].fresh_authentication?).to be(false)
      end).call(env_with_params("/"))
    end

    it "does not refresh the completion time when the user is fetched from the session" do
      session = {}
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
      end).call(env_with_params("/", login_params, 'rack.session' => session))

      @now = Time.at(1_000_900)
      build_app(lambda do |env|
        expect(env['warden'].authenticated?).to be(true)
        evidence = env['warden'].authentication_evidence
        expect(evidence[:method]).to eq("password")
        expect(evidence[:authenticated_at]).to eq(Time.at(1_000_000))
      end).call(env_with_params("/", {}, 'rack.session' => session))
    end

    it "does not refresh the completion time when the user is read repeatedly" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        @now = Time.at(1_000_900)
        env['warden'].user
        env['warden'].user
        expect(env['warden'].authentication_evidence[:authenticated_at]).to eq(Time.at(1_000_000))
      end).call(env_with_params("/", login_params))
    end

    it "clears the evidence on logout" do
      session = {}
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        env['warden'].logout
        expect(env['warden'].authentication_evidence).to be_nil
        expect(env['rack.session']).not_to have_key("warden.user.default.authentication")
      end).call(env_with_params("/", login_params, 'rack.session' => session))
    end
  end

  describe "fresh_authentication?" do
    it "is true when no requirements are given but evidence exists" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        expect(env['warden'].fresh_authentication?).to be(true)
      end).call(env_with_params("/", login_params))
    end

    it "is false when nobody is logged in" do
      build_app(lambda do |env|
        expect(env['warden'].fresh_authentication?).to be(false)
        expect(env['warden'].fresh_authentication?(:within => 300)).to be(false)
      end).call(env_with_params("/"))
    end

    it "does not let a weak method satisfy a strong method requirement" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        expect(env['warden'].fresh_authentication?(:via => :password)).to be(true)
        expect(env['warden'].fresh_authentication?(:via => :strong_auth)).to be(false)
      end).call(env_with_params("/", login_params))
    end

    it "accepts a list of allowed methods" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        expect(env['warden'].fresh_authentication?(:via => [:strong_auth, :password])).to be(true)
        expect(env['warden'].fresh_authentication?(:via => [:strong_auth])).to be(false)
      end).call(env_with_params("/", login_params))
    end

    it "keeps the original method when the user is restored from the session" do
      session = {}
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
      end).call(env_with_params("/", login_params, 'rack.session' => session))

      build_app(lambda do |env|
        expect(env['warden'].fresh_authentication?(:via => :password)).to be(true)
        expect(env['warden'].fresh_authentication?(:via => :strong_auth)).to be(false)
      end).call(env_with_params("/", {}, 'rack.session' => session))
    end

    it "treats an authentication exactly as old as the maximum age as fresh" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        @now = Time.at(1_000_000 + 600)
        expect(env['warden'].fresh_authentication?(:within => 600)).to be(true)
        @now = Time.at(1_000_000 + 601)
        expect(env['warden'].fresh_authentication?(:within => 600)).to be(false)
      end).call(env_with_params("/", login_params))
    end

    it "applies the age check to users served from the request cache" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        @now = Time.at(1_000_000 + 3_600)
        expect(env['warden'].user).to eq("Authenticated User")
        expect(env['warden'].authenticated?).to be(true)
        expect(env['warden'].fresh_authentication?(:within => 600)).to be(false)
      end).call(env_with_params("/", login_params))
    end

    it "applies the age check to users restored from the session" do
      session = {}
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
      end).call(env_with_params("/", login_params, 'rack.session' => session))

      @now = Time.at(1_000_000 + 3_600)
      build_app(lambda do |env|
        expect(env['warden'].authenticated?).to be(true)
        expect(env['warden'].fresh_authentication?(:within => 600)).to be(false)
        expect(env['warden'].fresh_authentication?(:within => 7_200)).to be(true)
      end).call(env_with_params("/", {}, 'rack.session' => session))
    end

    it "checks method and age together" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        expect(env['warden'].fresh_authentication?(:via => :password, :within => 600)).to be(true)
        expect(env['warden'].fresh_authentication?(:via => :strong_auth, :within => 600)).to be(false)
        @now = Time.at(1_000_000 + 3_600)
        expect(env['warden'].fresh_authentication?(:via => :password, :within => 600)).to be(false)
      end).call(env_with_params("/", login_params))
    end
  end

  describe "require_fresh_authentication!" do
    it "returns the user when the requirements are met" do
      build_app(lambda do |env|
        env['warden'].authenticate(:password)
        user = env['warden'].require_fresh_authentication!(:via => :password, :within => 600)
        expect(user).to eq("Authenticated User")
      end).call(env_with_params("/", login_params))
    end

    it "halts the request through the failure app when the requirements are not met" do
      response = build_app(lambda do |env|
        env['warden'].authenticate(:password)
        env['warden'].require_fresh_authentication!(:via => :strong_auth)
      end).call(env_with_params("/", login_params))

      expect(response.first).to eq(401)
    end

    it "keeps the normal login usable when the requirements are not met" do
      session = {}
      response = build_app(lambda do |env|
        env['warden'].authenticate(:password)
        env['warden'].require_fresh_authentication!(:via => :strong_auth)
      end).call(env_with_params("/", login_params, 'rack.session' => session))
      expect(response.first).to eq(401)

      build_app(lambda do |env|
        expect(env['warden'].authenticated?).to be(true)
        expect(env['warden'].user).to eq("Authenticated User")
      end).call(env_with_params("/", {}, 'rack.session' => session))
    end

    it "halts the request when the authentication is too old" do
      response = build_app(lambda do |env|
        env['warden'].authenticate(:password)
        @now = Time.at(1_000_000 + 3_600)
        env['warden'].require_fresh_authentication!(:within => 600)
      end).call(env_with_params("/", login_params))

      expect(response.first).to eq(401)
    end
  end
end
