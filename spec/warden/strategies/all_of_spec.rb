# encoding: utf-8
# frozen_string_literal: true

RSpec.describe "Warden::Strategies all_of groups" do
  before(:all) do
    load_strategies
    Warden::Strategies.all_of(:both_factors, :factor_one, :factor_two) do |first, other|
      first.id == other.id
    end
    Warden::Strategies.all_of(:factor_with_invalid, :factor_one, :invalid) do |first, other|
      first == other
    end
  end

  after(:all) do
    Warden::Strategies._all_of_strategies.delete(:both_factors)
    Warden::Strategies._all_of_strategies.delete(:factor_with_invalid)
  end

  after(:each) do
    Warden::Strategies._all_of_strategies.delete(:missing_member)
  end

  def authenticate_app(strategy = :both_factors, opts = {})
    lambda do |env|
      env['warden'].authenticate(strategy, opts)
      if env['warden'].authenticated?
        valid_response
      else
        [401, {'Content-Type' => 'text/plain'}, [env['warden'].message.to_s]]
      end
    end
  end

  describe "registration" do
    it "requires a block to compare subjects" do
      expect {
        Warden::Strategies.all_of(:no_block, :factor_one, :factor_two)
      }.to raise_error(ArgumentError, /needs a block to compare subjects/)
    end

    it "requires at least one member strategy" do
      expect {
        Warden::Strategies.all_of(:empty) { |_a, _b| true }
      }.to raise_error(ArgumentError, /needs at least one strategy/)
    end

    it "can be used as a default strategy for a scope" do
      configurator = proc do |config|
        config.scope_defaults :sudo, :strategies => [:both_factors]
      end
      app = lambda do |env|
        env['warden'].authenticate(:scope => :sudo)
        env['warden'].authenticated?(:sudo) ? valid_response : [401, {}, ["fail"]]
      end
      env = env_with_params("/",
                            'factor_one_id' => '1', 'factor_two_id' => '1')
      response = setup_rack(app, :configurator => configurator).call(env)
      expect(response.first).to eq(200)
    end
  end

  describe "authenticating" do
    it "logs the user in when all factors agree on the same subject" do
      app = lambda do |env|
        user = env['warden'].authenticate(:both_factors)
        expect(user).to be_a(FakeSubject)
        expect(user.id).to eq(1)
        expect(env['warden.spec.strategies']).to eq([:factor_one, :factor_two])
        valid_response
      end
      env = env_with_params("/",
                            'factor_one_id' => '1', 'factor_two_id' => '1')
      expect(setup_rack(app).call(env).first).to eq(200)
    end

    it "does not assume two Ruby objects are equal for the same subject" do
      expect(FakeSubject.new(7)).not_to eq(FakeSubject.new(7))

      app = lambda do |env|
        env['warden'].authenticate(:both_factors)
        expect(env['warden']).to be_authenticated
        valid_response
      end
      env = env_with_params("/",
                            'factor_one_id' => '7', 'factor_two_id' => '7')
      expect(setup_rack(app).call(env).first).to eq(200)
    end

    it "fails when the factors authenticate different subjects" do
      env = env_with_params("/",
                            'factor_one_id' => '1', 'factor_two_id' => '2')
      response = setup_rack(authenticate_app).call(env)
      expect(response.first).to eq(401)
      expect(env['warden'].message).to match(/different subjects/)
      expect(env['warden'].user).to be_nil
      expect(env['rack.session']).not_to have_key("warden.user.default.key")
    end

    it "runs every member before deciding on different subjects" do
      env = env_with_params("/",
                            'factor_one_id' => '1', 'factor_two_id' => '2')
      setup_rack(authenticate_app).call(env)
      expect(env['warden.spec.strategies']).to eq([:factor_one, :factor_two])
    end

    it "fails when a later factor fails after an earlier one succeeds" do
      env = env_with_params("/",
                            'factor_one_id' => '1',
                            'factor_two' => 'fail',
                            'factor_two_id' => '1')
      response = setup_rack(authenticate_app).call(env)
      expect(response.first).to eq(401)
      expect(env['warden'].message).to eq("Second factor failed")
      expect(env['warden'].user).to be_nil
      expect(env['rack.session']).not_to have_key("warden.user.default.key")
    end

    it "stops cascading once a member fails" do
      env = env_with_params("/",
                            'factor_one' => 'fail', 'factor_one_id' => '1',
                            'factor_two_id' => '1')
      setup_rack(authenticate_app).call(env)
      expect(env['warden.spec.strategies']).to eq([:factor_one])
    end

    it "fails when a member strategy is not valid for the request" do
      app = lambda do |env|
        env['warden'].authenticate(:factor_with_invalid)
        valid_response
      end
      env = env_with_params("/", 'factor_one_id' => '1')
      setup_rack(app).call(env)
      expect(env['warden']).not_to be_authenticated
      expect(env['warden'].winning_strategy.result).to eq(:failure)
      expect(env['rack.session']).not_to have_key("warden.user.default.key")
    end

    it "raises when a member strategy is not registered" do
      Warden::Strategies.all_of(:missing_member, :factor_one, :nope) do |a, b|
        a.id == b.id
      end
      app = lambda do |env|
        env['warden'].authenticate(:missing_member)
        valid_response
      end
      expect {
        setup_rack(app).call(env_with_params("/", 'factor_one_id' => '1'))
      }.to raise_error(RuntimeError, "Invalid strategy nope")
    end

    it "exposes the failing member result through the winning strategy" do
      env = env_with_params("/",
                            'factor_one_id' => '1',
                            'factor_two' => 'fail',
                            'factor_two_id' => '1')
      setup_rack(authenticate_app).call(env)
      group = env['warden'].winning_strategy
      expect(group).to be_a(Warden::Strategies::AllOf)
      expect(group.result).to eq(:failure)
      expect(group.message).to eq("Second factor failed")
    end

    it "does not run the after_authentication hook when a factor fails" do
      Warden::Manager.after_authentication do |_user, auth, _opts|
        auth.env['warden.spec.authentication_hook'] = true
      end
      env = env_with_params("/",
                            'factor_one_id' => '1',
                            'factor_two' => 'fail',
                            'factor_two_id' => '1')
      setup_rack(authenticate_app).call(env)
      expect(env['warden.spec.authentication_hook']).to be_nil
    ensure
      Warden::Manager._after_set_user.clear
    end

    it "runs the after_authentication hook only when every factor succeeds" do
      Warden::Manager.after_authentication do |_user, auth, _opts|
        auth.env['warden.spec.authentication_hook'] = true
      end
      env = env_with_params("/",
                            'factor_one_id' => '3', 'factor_two_id' => '3')
      setup_rack(authenticate_app).call(env)
      expect(env['warden.spec.authentication_hook']).to eq(true)
    ensure
      Warden::Manager._after_set_user.clear
    end

    it "stores the user in the session only when the group succeeds" do
      success_env = env_with_params("/",
                                    'factor_one_id' => '4',
                                    'factor_two_id' => '4')
      setup_rack(authenticate_app).call(success_env)
      expect(success_env['rack.session']["warden.user.default.key"]).to be_a(FakeSubject)

      failure_env = env_with_params("/",
                                    'factor_one_id' => '4',
                                    'factor_two' => 'fail',
                                    'factor_two_id' => '4')
      setup_rack(authenticate_app).call(failure_env)
      expect(failure_env['rack.session']["warden.user.default.key"]).to be_nil
    end

    it "keeps the legacy any-strategy chain behavior" do
      app = lambda do |env|
        env['warden'].authenticate(:factor_one, :factor_two)
        env['warden'].authenticated? ? valid_response : [401, {}, ["fail"]]
      end
      env = env_with_params("/",
                            'factor_one_id' => '1', 'factor_two_id' => '9')
      response = setup_rack(app).call(env)
      expect(response.first).to eq(200)
      expect(env['warden'].user.id).to eq(1)
    end

    it "does not treat the members as a group when listed separately" do
      app = lambda do |env|
        env['warden'].authenticate(:factor_one, :factor_two)
        valid_response
      end
      env = env_with_params("/",
                            'factor_one' => 'fail', 'factor_one_id' => '1',
                            'factor_two' => 'fail', 'factor_two_id' => '1')
      response = setup_rack(app).call(env)
      expect(response.first).to eq(200)
      expect(env['warden'].winning_strategy).to be_a(Warden::Strategies::Base)
      expect(env['warden'].winning_strategy).not_to be_a(Warden::Strategies::AllOf)
    end
  end

end
