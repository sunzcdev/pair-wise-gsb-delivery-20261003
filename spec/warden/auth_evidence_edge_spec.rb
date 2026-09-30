# encoding: utf-8
# frozen_string_literal: true

RSpec.describe "authentication evidence edge cases" do
  let(:clock) { Warden::Spec::Helpers::FakeClock.new(1000) }

  def fresh_request
    @env.delete("warden")
    @env['rack.session'] ||= {}
  end

  before(:each) do
    load_strategies
    @env = env_with_params
    @env['rack.session'] = {}
  end

  it "does not record evidence from a weak success when a strong method is required" do
    app = lambda { |env| env['warden'].authenticate(:pass, :clock => clock); valid_response }
    fresh_request
    setup_rack(app).call(@env)

    # A :pass (weak) success during a request demanding :otp must neither
    # establish evidence nor be treated as satisfying the requirement.
    fresh_request
    app2 = lambda do |env|
      returned = env['warden'].authenticate(:pass, :methods => :otp,
                                            :clock => clock)
      expect(returned).to be_nil
      expect(env['warden'].reauthenticated?(:methods => :otp, :clock => clock)).to eq(false)
      valid_response
    end
    setup_rack(app2).call(@env)
  end

  it "a non-bang step-up attempt that succeeds in the same request returns the user" do
    # Session with an existing weak login.
    app = lambda { |env| env['warden'].authenticate(:pass, :clock => clock); valid_response }
    fresh_request
    setup_rack(app).call(@env)

    # New request carries valid password credentials and asks for a fresh window.
    clock.advance(500)
    @env2 = env_with_params("/", "username" => "fred", "password" => "sekrit")
    @env2['rack.session'] = @env['rack.session']
    @env = @env2
    fresh_request

    result = nil
    app2 = lambda do |env|
      result = env['warden'].authenticate(:password, :max_age => 300, :clock => clock)
      valid_response
    end
    setup_rack(app2).call(@env)

    expect(result).to eq("Authenticated User")
    expect(@env['rack.session'][Warden::AuthEvidence::SESSION_KEY]["default"]).to eq(
      "method" => "password", "at" => 1500.0
    )
  end

  it "restores legacy logins (no evidence) as authenticated but not freshly verified" do
    @env['rack.session']['warden.user.default.key'] = "legacy user"

    app = lambda do |env|
      expect(env['warden'].authenticated?).to eq(true)
      expect(env['warden'].reauthenticated?(:max_age => 3600, :clock => clock)).to eq(false)
      expect(env['warden'].authentication_method).to be_nil
      valid_response
    end
    fresh_request
    setup_rack(app).call(@env)
  end

  it "checks the age boundary using the injected clock" do
    evidence = Warden::AuthEvidence.new(:otp, 1000)
    clock = Warden::Spec::Helpers::FakeClock.new(1000)

    expect(evidence.satisfies?(:max_age => 0, :clock => clock)).to eq(true)
    clock.advance(1)
    expect(evidence.satisfies?(:max_age => 0, :clock => clock)).to eq(false)
    expect(evidence.satisfies?(:max_age => 1, :clock => clock)).to eq(true)
  end
end

