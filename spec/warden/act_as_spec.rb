# encoding: utf-8
# frozen_string_literal: true

require 'rack/session'

RSpec.describe "temporary act as switches" do
  let(:agent)  { "support-agent" }
  let(:target) { "customer" }

  let(:warden) { env['warden'] }
  let(:env)    { env_with_params("/") }

  def run_app
    app = lambda do |e|
      e['warden'].set_user(agent)
      yield e
      valid_response
    end
    setup_rack(app).call(env)
  end

  before(:each) do
    RAM = Warden::Manager unless defined?(RAM)
    @hook_arrays = [:_before_act_as, :_after_act_as, :_after_stop_acting,
                    :_after_set_user, :_before_logout]
    @hook_arrays.each { |name| RAM.send(name).clear }
  end

  after(:each) do
    @hook_arrays.each { |name| RAM.send(name).clear }
  end

  describe "act_as" do
    it "makes the target the effective user only inside the block" do
      seen = nil
      run_app do |e|
        result = e['warden'].act_as(target) { |t, auth| [t, auth.user] }
        expect(result).to eq([target, target])
        seen = e['warden'].user
      end
      expect(seen).to eq(agent)
      expect(warden.user).to eq(agent)
    end

    it "exposes the real operator, current target and acting state" do
      inside = nil
      run_app do |e|
        e['warden'].act_as(target) do
          a = e['warden']
          inside = {
            :user        => a.user,
            :real_user   => a.real_user,
            :acting_user => a.acting_user,
            :acting      => a.acting?
          }
        end
      end
      expect(inside).to eq(:user => target, :real_user => agent,
                           :acting_user => target, :acting => true)
      expect(warden.real_user).to eq(agent)
      expect(warden.acting_user).to be_nil
      expect(warden).not_to be_acting
    end

    it "sees the target through authenticated? inside but not after" do
      run_app do |e|
        e['warden'].act_as(target) do
          expect(e['warden']).to be_authenticated
          expect(e['warden'].authenticated?).to eq(true)
        end
        expect(e['warden'].user).to eq(agent)
      end
    end

    it "returns the block value" do
      run_app do |e|
        expect(e['warden'].act_as(target) { :done }).to eq(:done)
      end
    end

    it "reports a nil real operator when nobody was logged in before switching" do
      app = lambda do |e|
        e['warden'].act_as(target) do
          expect(e['warden'].user).to eq(target)
          expect(e['warden'].real_user).to be_nil
          expect(e['warden']).to be_acting
        end
        expect(e['warden'].real_user).to be_nil
        valid_response
      end
      setup_rack(app).call(env)
      expect(warden.user).to be_nil
      expect(warden).not_to be_acting
    end

    it "requires a block" do
      run_app do |e|
        expect { e['warden'].act_as(target) }.to raise_error(LocalJumpError)
      end
    end
  end

  describe "session persistence" do
    it "serializes the real operator, never the target, into the session" do
      run_app do |e|
        e['warden'].act_as(target) {}
      end
      expect(env['rack.session']["warden.user.default.key"]).to eq(agent)
      expect(env['rack.session'].values).not_to include(target)
    end
  end

  describe "hooks" do
    it "runs before_act_as, after_act_as and after_stop_acting in order" do
      events = []
      RAM.before_act_as     { |t, a, o| events << [:before, t, a.real_user(o[:scope]), o[:scope]] }
      RAM.after_act_as      { |t, a, o| events << [:after, t, a.user(o[:scope]), o[:scope]] }
      RAM.after_stop_acting { |t, a, o| events << [:stop, t, a.real_user(o[:scope]), o[:scope]] }

      run_app do |e|
        e['warden'].act_as(target) { events << :block }
      end

      expect(events).to eq([
        [:before, target, agent, :default],
        [:after,  target, target, :default],
        :block,
        [:stop,   target, agent, :default]
      ])
    end

    it "is distinct from real login and logout callbacks" do
      set_events, logout_events = [], []
      RAM.after_set_user { |u, _a, o| set_events << [u, o[:event]] }
      RAM.before_logout  { |u, _a, o| logout_events << [u, o[:scope]] }

      run_app do |e|
        e['warden'].act_as(target) {}
      end

      expect(set_events).to eq([[agent, :set_user]])
      expect(logout_events).to be_empty
    end

    it "does not fire after_authentication for the target" do
      authenticated_users = []
      RAM.after_authentication { |u, _a, _o| authenticated_users << u }

      run_app do |e|
        e['warden'].act_as(target) {}
      end

      expect(authenticated_users).to be_empty
    end
  end

  describe "authorization" do
    it "aborts the switch when before_act_as raises ActAsDenied" do
      RAM.before_act_as { raise Warden::ActAsDenied, "not allowed" }

      block_ran = false
      expect do
        run_app { |e| e['warden'].act_as(target) { block_ran = true } }
      end.to raise_error(Warden::ActAsDenied)

      expect(block_ran).to eq(false)
      expect(warden.user).to eq(agent)
      expect(warden).not_to be_acting
    end

    it "does not install the target or run after hooks when denied" do
      after_fired, stop_fired = false, false
      RAM.before_act_as     { raise Warden::ActAsDenied }
      RAM.after_act_as      { after_fired = true }
      RAM.after_stop_acting { stop_fired = true }

      expect do
        run_app { |e| e['warden'].act_as(target) {} }
      end.to raise_error(Warden::ActAsDenied)

      expect(after_fired).to eq(false)
      expect(stop_fired).to eq(false)
      expect(warden.acting_user).to be_nil
      expect(warden).not_to be_acting
    end

    it "passes the real operator to the authorization callback" do
      seen_operator = nil
      RAM.before_act_as { |_t, a, o| seen_operator = a.real_user(o[:scope]) }

      run_app { |e| e['warden'].act_as(target) {} }

      expect(seen_operator).to eq(agent)
    end

    it "leaves the real identity intact when authorization throws :warden" do
      RAM.before_act_as { throw :warden, :scope => :default }
      block_ran = false

      run_app { |e| e['warden'].act_as(target) { block_ran = true } }

      expect(block_ran).to eq(false)
      expect(warden.user).to eq(agent)
      expect(warden).not_to be_acting
      expect(warden.acting_user).to be_nil
    end
  end

  describe "nesting" do
    it "authorizes every layer and restores each in turn" do
      middle = "lead-agent"
      calls, trace = [], []
      RAM.before_act_as { |t, _a, _o| calls << t }
      RAM.after_stop_acting { |t, _a, _o| trace << [:stop, t] }

      run_app do |e|
        e['warden'].act_as(middle) do
          trace << [:outer_user, e['warden'].user, e['warden'].real_user]
          e['warden'].act_as(target) do
            trace << [:inner_user, e['warden'].user, e['warden'].real_user,
                      e['warden'].acting_user]
          end
          trace << [:outer_again, e['warden'].user, e['warden'].real_user,
                    e['warden'].acting_user]
        end
      end

      expect(calls).to eq([middle, target])
      expect(trace).to eq([
        [:outer_user,  middle, agent],
        [:inner_user,  target, agent, target],
        [:stop, target],
        [:outer_again, middle, agent, middle],
        [:stop, middle]
      ])
      expect(warden.user).to eq(agent)
      expect(warden).not_to be_acting
    end
  end

  describe "cleanup" do
    it "restores the real operator when the block raises" do
      stop_target = nil
      RAM.after_stop_acting { |t, _a, _o| stop_target = t }

      expect do
        run_app do |e|
          e['warden'].act_as(target) { raise "boom" }
        end
      end.to raise_error(RuntimeError, "boom")

      expect(stop_target).to eq(target)
      expect(warden.user).to eq(agent)
      expect(warden).not_to be_acting
      expect(warden.acting_user).to be_nil
    end

    it "restores the real operator on an early return" do
      returned = nil
      runner = lambda do |proxy|
        proxy.act_as(target) do
          returned = proxy.user
          return :early
        end
      end

      run_app do |e|
        runner.call(e['warden'])
      end

      expect(returned).to eq(target)
      expect(warden.user).to eq(agent)
      expect(warden).not_to be_acting
    end

    it "leaves no frame behind after an inner raise in a nested switch" do
      expect do
        run_app do |e|
          e['warden'].act_as("lead") do
            e['warden'].act_as(target) { raise "nested boom" }
          end
        end
      end.to raise_error(RuntimeError, "nested boom")

      expect(warden).not_to be_acting
      expect(warden.acting_user).to be_nil
      expect(warden.user).to eq(agent)
    end
  end

  describe "scopes" do
    it "switches a single scope without touching other scopes" do
      run_app do |e|
        e['warden'].set_user("admin", :scope => :admin)
        e['warden'].act_as(target, :scope => :admin) do
          expect(e['warden'].user(:admin)).to eq(target)
          expect(e['warden'].real_user(:admin)).to eq("admin")
          expect(e['warden'].user(:default)).to eq(agent)
          expect(e['warden'].acting?(:admin)).to eq(true)
          expect(e['warden'].acting?(:default)).to eq(false)
        end
      end

      expect(warden.user(:admin)).to eq("admin")
      expect(warden.user(:default)).to eq(agent)
      expect(warden.acting?(:admin)).to eq(false)
    end

    it "keeps independent switches on different scopes from leaking" do
      admin_target = "admin-target"
      run_app do |e|
        e['warden'].set_user("admin", :scope => :admin)
        e['warden'].act_as(admin_target, :scope => :admin) do
          e['warden'].act_as(target, :scope => :default) do
            expect(e['warden'].user(:default)).to eq(target)
            expect(e['warden'].user(:admin)).to eq(admin_target)
            expect(e['warden'].acting?(nil)).to eq(true)
          end
          expect(e['warden'].user(:default)).to eq(agent)
          expect(e['warden'].user(:admin)).to eq(admin_target)
        end
      end

      expect(warden.user(:default)).to eq(agent)
      expect(warden.user(:admin)).to eq("admin")
      expect(warden.acting?(nil)).to eq(false)
    end

    it "does not serialize the target for a scoped switch" do
      run_app do |e|
        e['warden'].set_user("admin", :scope => :admin)
        e['warden'].act_as(target, :scope => :admin) {}
      end
      expect(env['rack.session']["warden.user.admin.key"]).to eq("admin")
      expect(env['rack.session'].values).not_to include(target)
    end
  end
end
