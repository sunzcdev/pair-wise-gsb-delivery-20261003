# encoding: utf-8
# frozen_string_literal: true

require 'rack/session'

RSpec.describe "temporary on-behalf switch_user" do
  before(:all) do
    load_strategies
  end

  before(:each) do
    @manager = Warden::Manager
    @manager._on_switch_user.clear
    @manager._before_switch_user.clear
    @manager._after_switch_user.clear
    @manager._after_set_user.clear
    @manager._before_logout.clear
  end

  after(:each) do
    @manager._on_switch_user.clear
    @manager._before_switch_user.clear
    @manager._after_switch_user.clear
    @manager._after_set_user.clear
    @manager._before_logout.clear
  end

  def run_with_warden(&block)
    app = lambda do |env|
      env['warden.spec.result'] = block.call(env['warden'])
      valid_response
    end
    env = env_with_params
    setup_rack(app).call(env)
    env['warden.spec.result']
  end

  describe "switch_user" do
    it "requires a block" do
      expect {
        run_with_warden { |w| w.switch_user("target") }
      }.to raise_error(ArgumentError)
    end

    it "makes the target the effective user inside the block" do
      @manager.on_switch_user { true }
      result = run_with_warden do |w|
        w.switch_user("target") do
          [w.user, w.switched?]
        end
      end
      expect(result).to eq(["target", true])
    end

    it "yields the target user" do
      @manager.on_switch_user { true }
      yielded = run_with_warden do |w|
        w.switch_user("target") { |t| t }
      end
      expect(yielded).to eq("target")
    end

    it "returns the block result" do
      @manager.on_switch_user { true }
      result = run_with_warden do |w|
        w.switch_user("target") { :from_the_block }
      end
      expect(result).to eq(:from_the_block)
    end

    it "restores the previous identity after the block" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.switch_user("target") { }
        expect(w.user).to be_nil
        expect(w.switched?).to eq(false)
      end
    end

    it "restores the operator when a real user was authenticated" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("target") do
          expect(w.user).to eq("target")
        end
        expect(w.user).to eq("agent")
        expect(w.real_user).to eq("agent")
        expect(w.switched?).to eq(false)
      end
    end

    it "honours the given scope" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent", :scope => :admin)
        w.switch_user("customer", :scope => :admin) do
          expect(w.user(:admin)).to eq("customer")
          expect(w.real_user(:admin)).to eq("agent")
          expect(w.switched?(:admin)).to eq(true)
        end
        expect(w.user(:admin)).to eq("agent")
      end
    end
  end

  describe "persistence" do
    it "does not store the target user in the persistent session" do
      @manager.on_switch_user { true }
      env = env_with_params
      app = lambda do |e|
        e['warden'].switch_user("target") { }
        valid_response
      end
      setup_rack(app).call(env)
      expect(env['rack.session']['warden.user.default.key']).to be_nil
    end

    it "does not renew the session options" do
      @manager.on_switch_user { true }
      env = env_with_params
      env['rack.session.options'] = { :renew => false }
      app = lambda do |e|
        e['warden'].switch_user("target") { }
        valid_response
      end
      setup_rack(app).call(env)
      expect(env['rack.session.options'][:renew]).to eq(false)
    end

    it "does not fire after_set_user for the switch" do
      @manager.on_switch_user { true }
      events = []
      @manager.after_set_user { |u, _a, _o| events << u }
      run_with_warden do |w|
        w.switch_user("target") { }
      end
      expect(events).to eq([])
    end
  end

  describe "authorization" do
    it "raises SwitchUserNotAuthorized when no callback is registered" do
      expect {
        run_with_warden { |w| w.switch_user("target") { } }
      }.to raise_error(Warden::SwitchUserNotAuthorized)
    end

    it "raises when a callback denies consent" do
      @manager.on_switch_user { false }
      expect {
        run_with_warden { |w| w.switch_user("target") { } }
      }.to raise_error(Warden::SwitchUserNotAuthorized)
    end

    it "raises when any matching callback denies consent" do
      @manager.on_switch_user { true }
      @manager.on_switch_user { false }
      expect {
        run_with_warden { |w| w.switch_user("target") { } }
      }.to raise_error(Warden::SwitchUserNotAuthorized)
    end

    it "proceeds when every matching callback consents" do
      @manager.on_switch_user { true }
      @manager.on_switch_user { 1 }
      expect {
        run_with_warden { |w| w.switch_user("target") { } }
      }.not_to raise_error
    end

    it "passes the operator, target, proxy and options to the callback" do
      seen = nil
      @manager.on_switch_user do |operator, target, auth, opts|
        seen = [operator, target, auth.class, opts[:scope]]
        true
      end
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("target", :scope => :default) { }
      end
      expect(seen).to eq(["agent", "target", Warden::Proxy, :default])
    end

    it "only counts callbacks matching the scope as applicable" do
      @manager.on_switch_user(:scope => :admin) { false }
      expect {
        run_with_warden do |w|
          w.switch_user("target", :scope => :user) { }
        end
      }.to raise_error(Warden::SwitchUserNotAuthorized)

      @manager.on_switch_user(:scope => :user) { true }
      expect {
        run_with_warden do |w|
          w.switch_user("target", :scope => :user) { }
        end
      }.not_to raise_error
    end

    it "accepts an array of scopes in the condition" do
      @manager.on_switch_user(:scope => [:user, :admin]) { true }
      expect {
        run_with_warden { |w| w.switch_user("target", :scope => :admin) { } }
      }.not_to raise_error
    end

    it "leaves the effective user untouched when denied" do
      @manager.on_switch_user { false }
      run_with_warden do |w|
        w.set_user("agent")
        begin
          w.switch_user("target") { }
        rescue Warden::SwitchUserNotAuthorized
        end
        expect(w.user).to eq("agent")
        expect(w.switched?).to eq(false)
      end
    end
  end

  describe "nesting" do
    it "requires authorization for each layer" do
      calls = []
      @manager.on_switch_user do |operator, target, _auth, _opts|
        calls << [operator, target]
        true
      end
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("customer") do
          w.switch_user("sub_account") do
            expect(w.user).to eq("sub_account")
          end
        end
      end
      expect(calls).to eq([
        ["agent", "customer"],
        ["customer", "sub_account"]
      ])
    end

    it "keeps real_user pointed at the genuine operator through layers" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("customer") do
          expect(w.real_user).to eq("agent")
          w.switch_user("sub_account") do
            expect(w.user).to eq("sub_account")
            expect(w.real_user).to eq("agent")
          end
          expect(w.user).to eq("customer")
          expect(w.real_user).to eq("agent")
        end
      end
    end

    it "raises and unwinds the inner layer when an inner switch is denied" do
      checks = []
      @manager.on_switch_user do |_operator, target, _auth, _opts|
        checks << target
        target != "denied"
      end
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("customer") do
          expect {
            w.switch_user("denied") { }
          }.to raise_error(Warden::SwitchUserNotAuthorized)
          expect(w.user).to eq("customer")
          expect(w.real_user).to eq("agent")
          expect(w.switched?).to eq(true)
        end
        expect(checks).to eq(["customer", "denied"])
      end
    end
  end

  describe "cleanup" do
    it "restores identity when the block raises" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        begin
          w.switch_user("target") { raise "boom" }
        rescue RuntimeError
        end
        expect(w.user).to be_nil
        expect(w.switched?).to eq(false)
      end
    end

    it "restores identity on a non-local return from the block" do
      @manager.on_switch_user { true }
      observed = catch(:early) do
        run_with_warden do |w|
          w.switch_user("target") do
            expect(w.user).to eq("target")
            expect(w.switched?).to eq(true)
            throw(:early, :done)
          end
        end
      end
      expect(observed).to eq(:done)
    end

    it "keeps different scopes independent" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent", :scope => :admin)
        w.set_user("member", :scope => :user)
        w.switch_user("customer", :scope => :user) do
          expect(w.user(:user)).to eq("customer")
          expect(w.real_user(:user)).to eq("member")
          expect(w.user(:admin)).to eq("agent")
          expect(w.switched?(:user)).to eq(true)
          expect(w.switched?(:admin)).to eq(false)
        end
        expect(w.user(:user)).to eq("member")
        expect(w.user(:admin)).to eq("agent")
      end
    end

    it "does not leak a switch from one scope into another when leaving" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent", :scope => :admin)
        w.switch_user("customer", :scope => :admin) do
          w.switch_user("member", :scope => :user) do
            expect(w.switched?(:admin)).to eq(true)
            expect(w.switched?(:user)).to eq(true)
          end
          expect(w.switched?(:user)).to eq(false)
          expect(w.switched?(:admin)).to eq(true)
        end
        expect(w.switched?(:admin)).to eq(false)
        expect(w.switched?(:user)).to eq(false)
      end
    end
  end

  describe "authenticated?" do
    it "does not treat a switched scope as a credential-authenticated user" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.switch_user("target") do
          expect(w.user).to eq("target")
          expect(w).not_to be_authenticated
          expect(w).to be_unauthenticated
        end
      end
    end

    it "still reports a real authenticated operator in the scope" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("target") do
          expect(w).to be_authenticated
          expect(w.user).to eq("target")
        end
      end
    end

    it "does not run credential strategies while switched" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.switch_user("target") do
          expect(w.authenticate(:pass)).to be_nil
          expect(w.user).to eq("target")
        end
      end
    end
  end

  describe "logout" do
    it "logs out the real operator once but never fires before_logout for the temporary target" do
      @manager.on_switch_user { true }
      logouts = []
      @manager.before_logout { |user, _auth, opts| logouts << [user, opts[:scope]] }
      env = env_with_params
      app = lambda do |e|
        e['warden'].switch_user("target") do
          e['warden'].logout
        end
        valid_response
      end
      setup_rack(app).call(env)
      # There is no real operator here, so no before_logout should fire and
      # the temporary target must never appear as a logged-out user.
      expect(logouts.map(&:first)).not_to include("target")
      expect(env['rack.session']['warden.user.default.key']).to be_nil
    end

    it "logs out the real operator exactly once, not the temporary target" do
      @manager.on_switch_user { true }
      logouts = []
      @manager.before_logout { |user, _auth, opts| logouts << [user, opts[:scope]] }
      env = env_with_params
      app = lambda do |e|
        e['warden'].set_user("agent")
        e['warden'].switch_user("target") do
          e['warden'].logout
        end
        valid_response
      end
      setup_rack(app).call(env)
      expect(logouts).to eq([["agent", :default]])
      expect(env['rack.session']['warden.user.default.key']).to be_nil
    end

    it "leaves real_user and switched? usable after a scoped logout during a switch" do
      @manager.on_switch_user { true }
      run_with_warden do |w|
        w.set_user("agent")
        w.switch_user("target") do
          w.logout
          expect(w.switched?).to eq(false)
          expect(w.real_user).to be_nil
          expect(w.user).to be_nil
        end
        expect(w.real_user).to be_nil
      end
    end
  end

  describe "hooks" do
    it "runs before_switch_user on entry and after_switch_user on exit" do
      @manager.on_switch_user { true }
      events = []
      @manager.before_switch_user { |user, auth, opts| events << [:before, user, auth.switched?(opts[:scope])] }
      @manager.after_switch_user  { |user, auth, opts| events << [:after, user, auth.switched?(opts[:scope])] }
      run_with_warden do |w|
        w.switch_user("target") do
          events << :inside
        end
      end
      expect(events).to eq([
        [:before, "target", true],
        :inside,
        [:after, "target", false]
      ])
    end

    it "always runs after_switch_user when the block raises" do
      @manager.on_switch_user { true }
      events = []
      @manager.after_switch_user { |user, _auth, _opts| events << user }
      run_with_warden do |w|
        begin
          w.switch_user("target") { raise "boom" }
        rescue RuntimeError
        end
      end
      expect(events).to eq(["target"])
    end

    it "does not run switch hooks when authorization is denied" do
      @manager.on_switch_user { false }
      events = []
      @manager.before_switch_user { |_u, _a, _o| events << :before }
      @manager.after_switch_user  { |_u, _a, _o| events << :after }
      begin
        run_with_warden { |w| w.switch_user("target") { } }
      rescue Warden::SwitchUserNotAuthorized
      end
      expect(events).to eq([])
    end

    it "filters switch hooks by scope" do
      @manager.on_switch_user { true }
      events = []
      @manager.before_switch_user(:scope => :other) { events << :other }
      @manager.before_switch_user(:scope => :default) { events << :default }
      run_with_warden do |w|
        w.switch_user("target") { }
      end
      expect(events).to eq([:default])
    end
  end
end
