# encoding: utf-8
# frozen_string_literal: true

class AllOfSpecUser
  attr_reader :id

  def initialize(id)
    @id = id
  end
end

RSpec.describe Warden::Strategies::AllOf do
  before(:all) do
    Warden::Strategies.add(:all_of_factor_one) do
      def authenticate!
        if params['factor_one']
          success!(AllOfSpecUser.new(params['factor_one']))
        else
          pass
        end
      end
    end

    Warden::Strategies.add(:all_of_factor_two) do
      def authenticate!
        if params['factor_two'] == 'invalid'
          fail!('second factor rejected')
        elsif params['factor_two']
          success!(AllOfSpecUser.new(params['factor_two']))
        else
          pass
        end
      end
    end

    Warden::Strategies.add_all_of(:all_of_two_factors, [:all_of_factor_one, :all_of_factor_two]) do |user_a, user_b|
      user_a.id == user_b.id
    end
  end

  before(:each) do
    RAM = Warden::Manager unless defined?(RAM)
    RAM._after_set_user.clear
  end

  after(:each) do
    RAM._after_set_user.clear
  end

  def all_of_app
    lambda do |env|
      env['warden'].authenticate(:all_of_two_factors)
      valid_response
    end
  end

  it "requires at least one member strategy" do
    expect {
      Warden::Strategies.add_all_of(:all_of_empty, []) { |a, b| a == b }
    }.to raise_error(ArgumentError, /at least one member strategy/)
  end

  it "requires an application provided subject matcher" do
    expect {
      Warden::Strategies.add_all_of(:all_of_without_matcher, [:all_of_factor_one])
    }.to raise_error(ArgumentError, /subject matcher/)
  end

  it "authenticates when every factor succeeds and identifies the same subject" do
    captured = nil
    RAM.after_authentication { |user, auth, opts| captured = user }

    env = env_with_params("/", :factor_one => "42", :factor_two => "42")
    setup_rack(all_of_app).call(env)

    expect(env['warden']).to be_authenticated
    expect(env['warden'].user).to be_a(AllOfSpecUser)
    expect(env['warden'].user.id).to eq("42")
    expect(env['rack.session']['warden.user.default.key']).not_to be_nil
    expect(captured).to be(env['warden'].user)
  end

  it "does not authenticate when the factors identify different subjects" do
    hook_ran = false
    RAM.after_authentication { |user, auth, opts| hook_ran = true }

    env = env_with_params("/", :factor_one => "42", :factor_two => "43")
    setup_rack(all_of_app).call(env)

    expect(env['warden']).not_to be_authenticated
    expect(env['warden'].user).to be_nil
    expect(env['warden'].winning_strategy.message).to eq("Authentication strategies did not identify the same subject")
    expect(env['rack.session']).not_to have_key('warden.user.default.key')
    expect(hook_ran).to be false
  end

  it "does not leave a half authenticated session when a later factor fails" do
    hook_ran = false
    RAM.after_authentication { |user, auth, opts| hook_ran = true }

    env = env_with_params("/", :factor_one => "42", :factor_two => "invalid")
    setup_rack(all_of_app).call(env)

    expect(env['warden']).not_to be_authenticated
    expect(env['warden'].user).to be_nil
    expect(env['warden'].winning_strategy.message).to eq("second factor rejected")
    expect(env['rack.session']).not_to have_key('warden.user.default.key')
    expect(hook_ran).to be false
  end

  it "cascades to the next strategy when a factor does not apply" do
    app = lambda do |env|
      env['warden'].authenticate(:all_of_two_factors, :pass)
      valid_response
    end

    env = env_with_params("/", :factor_one => "42")
    setup_rack(app).call(env)

    expect(env['warden']).to be_authenticated
    expect(env['warden'].user).to eq("Valid User")
  end

  it "keeps the regular any-of behavior for plain strategies" do
    app = lambda do |env|
      env['warden'].authenticate(:password, :pass)
      valid_response
    end

    env = env_with_params("/")
    setup_rack(app).call(env)

    expect(env['warden']).to be_authenticated
    expect(env['warden'].user).to eq("Valid User")
    expect(env['warden.spec.strategies']).to eq([:password, :pass])
  end
end
