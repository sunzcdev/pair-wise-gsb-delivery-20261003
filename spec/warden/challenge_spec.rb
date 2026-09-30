# encoding: utf-8
# frozen_string_literal: true

RSpec.describe Warden::Challenge do
  it "renders a bare scheme" do
    expect(Warden::Challenge.new("Basic").to_s).to eq("Basic")
  end

  it "accepts a symbol as the scheme" do
    expect(Warden::Challenge.new(:Bearer).to_s).to eq("Bearer")
  end

  it "renders token param values without quotes" do
    expect(Warden::Challenge.new("Basic", :realm => "api").to_s).to eq("Basic realm=api")
  end

  it "renders multiple params separated by commas" do
    challenge = Warden::Challenge.new("Bearer", :realm => "api", :error => "invalid_token")
    expect(challenge.to_s).to eq("Bearer realm=api, error=invalid_token")
  end

  it "quotes param values that are not tokens" do
    expect(Warden::Challenge.new("Basic", :realm => "my api").to_s).to eq('Basic realm="my api"')
  end

  it "keeps commas inside a quoted param value from breaking boundaries" do
    challenge = Warden::Challenge.new("Digest", :realm => "api", :qop => "auth,auth-int")
    expect(challenge.to_s).to eq('Digest realm=api, qop="auth,auth-int"')
  end

  it "escapes quotes and backslashes inside quoted param values" do
    challenge = Warden::Challenge.new("Basic", :realm => 'say "hi"\ok')
    expect(challenge.to_s).to eq('Basic realm="say \"hi\"\\\\ok"')
  end

  it "renders an empty param value as an empty quoted string" do
    expect(Warden::Challenge.new("Basic", :realm => "").to_s).to eq('Basic realm=""')
  end

  it "renders a token68 challenge" do
    expect(Warden::Challenge.new("Bearer", {}, "abc.def~/+=").to_s).to eq("Bearer abc.def~/+=")
  end

  it "rejects a challenge carrying both token68 and params" do
    expect {
      Warden::Challenge.new("Bearer", { :realm => "api" }, "abc=")
    }.to raise_error(ArgumentError)
  end

  it "rejects invalid schemes" do
    ["", "Bad Scheme", "Ba\"sic", "Basic,auth"].each do |scheme|
      expect { Warden::Challenge.new(scheme) }.to raise_error(ArgumentError)
    end
  end

  it "rejects invalid token68 data" do
    ["has space", "bad!token", "=leading"].each do |token68|
      expect { Warden::Challenge.new("Bearer", {}, token68) }.to raise_error(ArgumentError)
    end
  end

  it "rejects invalid param names" do
    expect {
      Warden::Challenge.new("Basic", "bad name" => "value")
    }.to raise_error(ArgumentError)
  end

  it "rejects param values containing control characters" do
    expect {
      Warden::Challenge.new("Basic", :realm => "api\r\nInjected: yes")
    }.to raise_error(ArgumentError)
  end
end

RSpec.describe "WWW-Authenticate challenges" do
  before(:each) do
    RAS = Warden::Strategies unless defined?(RAS)
    Warden::Strategies.clear!
  end

  after(:all) do
    load_strategies
  end

  def add_challenge_strategy(name, *challenges)
    RAS.add(name) do
      define_method(:authenticate!) do
        challenges.each { |args| challenge!(*args) }
        fail
      end
    end
  end

  it "sends every challenge declared for the scope when authentication fails" do
    add_challenge_strategy(:basic_auth, ["Basic", { :realm => "api" }])
    add_challenge_strategy(:bearer_token, ["Bearer", { :realm => "api", :error => "invalid_token" }])

    app = lambda do |env|
      env['warden'].authenticate!(:basic_auth, :bearer_token)
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[0]).to eq(401)
    expect(result[1]['WWW-Authenticate']).to eq('Basic realm=api, Bearer realm=api, error=invalid_token')
  end

  it "preserves quoting, escaping and commas inside params across multiple challenges" do
    add_challenge_strategy(:digest_auth, ["Digest", { :realm => 'my "api"', :qop => "auth,auth-int" }])
    add_challenge_strategy(:basic_auth, ["Basic", { :realm => "api" }])

    app = lambda do |env|
      env['warden'].authenticate!(:digest_auth, :basic_auth)
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[1]['WWW-Authenticate']).to eq('Digest realm="my \"api\"", qop="auth,auth-int", Basic realm=api')
  end

  it "supports token68 challenges" do
    add_challenge_strategy(:bearer_token, ["Bearer", {}, "some-token68="])

    app = lambda do |env|
      env['warden'].authenticate!(:bearer_token)
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[1]['WWW-Authenticate']).to eq("Bearer some-token68=")
  end

  it "does not add the header when no strategy declared a challenge" do
    RAS.add(:plain_fail) do
      def authenticate!
        fail
      end
    end

    app = lambda do |env|
      env['warden'].authenticate!(:plain_fail)
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[0]).to eq(401)
    expect(result[1]['WWW-Authenticate']).to be_nil
  end

  it "does not attempt to log in when collecting challenges" do
    app = lambda do |env|
      env['warden'].challenges
      [200, { "Content-Type" => "text/plain" }, ["ok"]]
    end

    env = env_with_params
    result = setup_rack(app).call(env)
    expect(result[0]).to eq(200)
    expect(env['warden.spec.strategies']).to be_nil
    expect(env['warden'].authenticated?).to be(false)
  end

  it "does not mix challenges from other scopes into the failure response" do
    add_challenge_strategy(:admin_basic, ["Basic", { :realm => "admin" }])
    add_challenge_strategy(:user_bearer, ["Bearer"])

    app = lambda do |env|
      env['warden'].authenticate(:admin_basic, :scope => :admin)
      env['warden'].authenticate!(:user_bearer)
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[1]['WWW-Authenticate']).to eq("Bearer")
  end

  it "keeps the original behavior when authentication succeeded" do
    RAS.add(:winner) do
      def authenticate!
        challenge!("Basic", :realm => "api")
        success!("A User")
      end
    end

    app = lambda do |env|
      env['warden'].authenticate(:winner)
      throw(:warden)
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[0]).to eq(401)
    expect(result[1]['WWW-Authenticate']).to be_nil
  end

  it "leaves custom failure responses untouched" do
    add_challenge_strategy(:basic_auth, ["Basic", { :realm => "api" }])

    app = lambda do |env|
      env['warden'].authenticate(:basic_auth)
      env['warden'].custom_failure!
      [401, { "Content-Type" => "text/plain" }, ["Fail From The App"]]
    end

    result = setup_rack(app).call(env_with_params)
    expect(result[0]).to eq(401)
    expect(result[2]).to eq(["Fail From The App"])
    expect(result[1]['WWW-Authenticate']).to be_nil
  end

  it "does not override a WWW-Authenticate header set by the failure app" do
    add_challenge_strategy(:basic_auth, ["Basic", { :realm => "api" }])

    fail_app = lambda do |_env|
      [401, { "WWW-Authenticate" => "Custom" }, ["fail"]]
    end
    app = lambda do |env|
      env['warden'].authenticate!(:basic_auth)
    end

    result = setup_rack(app, :failure_app => fail_app).call(env_with_params)
    expect(result[1]['WWW-Authenticate']).to eq("Custom")
  end

  it "does not add the header to non-401 failure responses" do
    add_challenge_strategy(:basic_auth, ["Basic", { :realm => "api" }])

    fail_app = lambda do |_env|
      [403, { "Content-Type" => "text/plain" }, ["forbidden"]]
    end
    app = lambda do |env|
      env['warden'].authenticate!(:basic_auth)
    end

    result = setup_rack(app, :failure_app => fail_app).call(env_with_params)
    expect(result[0]).to eq(403)
    expect(result[1]['WWW-Authenticate']).to be_nil
  end

  it "rejects invalid challenge data before the response is generated" do
    RAS.add(:bad_challenge) do
      def authenticate!
        challenge!("Bad Scheme")
      end
    end

    app = lambda do |env|
      env['warden'].authenticate!(:bad_challenge)
    end

    expect { setup_rack(app).call(env_with_params) }.to raise_error(ArgumentError)
  end
end
