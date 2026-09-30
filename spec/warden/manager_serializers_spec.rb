# encoding: utf-8
# frozen_string_literal: true
RSpec.describe Warden::Manager do
  def build_manager(&block)
    config = Warden::Config.new
    block.call(config)
    manager = Warden::Manager.allocate
    manager.config = config
    manager
  end

  def proxy_for(manager, session = {})
    env = env_with_params
    env['rack.session'] = session
    Warden::Proxy.new(env, manager)
  end

  describe "per Manager serializers" do
    it "does not define instance configured methods on the shared SessionSerializer class" do
      build_manager do |config|
        config.serialize_into_session { |user| "manager:#{user}" }
        config.serialize_from_session { |key| key }
        config.serialize_into_session(:admin) { |user| "admin:#{user}" }
        config.serialize_from_session(:admin) { |key| key }
      end

      expect(Warden::SessionSerializer).not_to be_method_defined(:admin_serialize)
      expect(Warden::SessionSerializer).not_to be_method_defined(:admin_deserialize)
    end

    it "uses a different serializer class for each Manager" do
      one = build_manager { |config| config.serialize_into_session { |user| user } }
      two = build_manager { |config| config.serialize_into_session { |user| user } }

      expect(proxy_for(one).session_serializer.class).not_to eq(proxy_for(two).session_serializer.class)
    end

    it "keeps the default SessionSerializer class when no Manager serializers are configured" do
      manager = build_manager { }
      expect(proxy_for(manager).session_serializer).to be_an_instance_of(Warden::SessionSerializer)
    end

    it "serializes the same user scope differently in two applications mounted in one process" do
      app_one = build_manager do |config|
        config.serialize_into_session { |user| "one-#{user.id}" }
        config.serialize_from_session { |key| key.is_a?(String) && key.start_with?("one-") ? key.sub(/^one-/, "") : nil }
      end
      app_two = build_manager do |config|
        config.serialize_into_session { |user| ["two", user.id] }
        config.serialize_from_session { |key| key.is_a?(Array) && key.first == "two" ? key.last : nil }
      end

      user = double("user", :id => "42")
      session_one = {}
      session_two = {}

      proxy_for(app_one, session_one).set_user(user, :scope => :user)
      proxy_for(app_two, session_two).set_user(user, :scope => :user)

      expect(session_one["warden.user.user.key"]).to eq("one-42")
      expect(session_two["warden.user.user.key"]).to eq(["two", "42"])

      expect(proxy_for(app_one, session_one).user(:user)).to eq("42")
      expect(proxy_for(app_two, session_two).user(:user)).to eq("42")
      expect(proxy_for(app_one, session_two).user(:user)).to be_nil
      expect(proxy_for(app_two, session_one).user(:user)).to be_nil
    end

    it "keeps interleaved requests from the two applications isolated" do
      app_one = build_manager do |config|
        config.serialize_into_session { |user| { :app => :one, :id => user.id } }
        config.serialize_from_session { |key| key[:id] if key[:app] == :one }
      end
      app_two = build_manager do |config|
        config.serialize_into_session { |user| { :app => :two, :id => user.id } }
        config.serialize_from_session { |key| key[:id] if key[:app] == :two }
      end

      user = double("user", :id => 7)
      sessions = { :one => {}, :two => {} }
      proxies = []

      proxies << proxy_for(app_one, sessions[:one])
      proxies << proxy_for(app_two, sessions[:two])
      proxies.first.set_user(user, :scope => :user)
      proxies.last.set_user(user, :scope => :user)
      proxies << proxy_for(app_one, sessions[:one])
      proxies << proxy_for(app_two, sessions[:two])

      expect(sessions[:one]["warden.user.user.key"]).to eq(:app => :one, :id => 7)
      expect(sessions[:two]["warden.user.user.key"]).to eq(:app => :two, :id => 7)
      expect(proxies[2].user(:user)).to eq(7)
      expect(proxies[3].user(:user)).to eq(7)
    end

    it "freezes the serialization protocol once a Proxy is created" do
      manager = build_manager do |config|
        config.serialize_into_session { |user| "v1:#{user}" }
      end

      existing_proxy = proxy_for(manager)

      manager.config.serialize_into_session { |user| "v2:#{user}" }

      session = {}
      env = existing_proxy.env
      env['rack.session'] = session
      existing_proxy.session_serializer.store("42", :default)
      expect(session["warden.user.default.key"]).to eq("v1:42")
    end

    it "isolates two Rack applications mounted in the same process over real requests" do
      logins = Hash.new(0)
      build_app = lambda do |app_name|
        lambda do |env|
          unless env['warden'].authenticated?(:user)
            env['warden'].set_user("42", :scope => :user, :event => :authentication)
            logins[app_name] += 1
          end
          [200, {}, [env['warden'].user(:user).inspect]]
        end
      end

      builder = Rack::Builder.new
      builder.use Warden::Spec::Helpers::Session
      mapped = Rack::URLMap.new(
        "/one" => Warden::Manager.new(build_app.call(:one)) do |config|
          config.serialize_into_session { |user| "one-#{user}" }
          config.serialize_from_session { |key| key.is_a?(String) ? key.sub(/^one-/, "") : nil }
        end,
        "/two" => Warden::Manager.new(build_app.call(:two)) do |config|
          config.serialize_into_session { |user| ["two", user] }
          config.serialize_from_session { |key| key.is_a?(Array) && key.first == "two" ? key.last : nil }
        end
      )
      builder.run mapped
      stack = builder.to_app

      session_one = {}
      response_one = stack.call(env_with_params("/one", {}).merge('rack.session' => session_one))
      expect(response_one.last).to eq(["\"42\""])
      expect(session_one["warden.user.user.key"]).to eq("one-42")

      session_two = {}
      response_two = stack.call(env_with_params("/two", {}).merge('rack.session' => session_two))
      expect(response_two.last).to eq(["\"42\""])
      expect(session_two["warden.user.user.key"]).to eq(["two", "42"])

      expect(stack.call(env_with_params("/one", {}).merge('rack.session' => session_one)).last).to eq(["\"42\""])
      expect(stack.call(env_with_params("/two", {}).merge('rack.session' => session_two)).last).to eq(["\"42\""])
      expect(logins).to eq(:one => 1, :two => 1)

      foreign_one = stack.call(env_with_params("/two", {}).merge('rack.session' => session_one.dup))
      foreign_two = stack.call(env_with_params("/one", {}).merge('rack.session' => session_two.dup))
      expect(foreign_one.last).to eq(["\"42\""])
      expect(foreign_two.last).to eq(["\"42\""])
      expect(logins).to eq(:one => 2, :two => 2)
    end
  end

  describe "fallback to class level serializers" do
    after(:each) do
      Warden::Manager.serialize_into_session { |user| user }
      Warden::Manager.serialize_from_session { |key| key }
      [:admin_serialize, :admin_deserialize].each do |name|
        Warden::SessionSerializer.send(:remove_method, name) if Warden::SessionSerializer.method_defined?(name)
      end
    end

    it "falls back to the class level default serializer" do
      Warden::Manager.serialize_into_session { |user| "class:#{user}" }
      Warden::Manager.serialize_from_session { |key| key.sub(/^class:/, "") }

      manager = build_manager { |config| config.serialize_into_session(:admin) { |user| user } }
      serializer = proxy_for(manager).session_serializer

      session = {}
      serializer.instance_variable_get(:@env)['rack.session'] = session
      serializer.store("42", :default)
      expect(session["warden.user.default.key"]).to eq("class:42")

      session["warden.user.default.key"] = "class:42"
      expect(serializer.fetch(:default)).to eq("42")
    end

    it "prefers a Manager default serializer over the class level default serializer" do
      Warden::Manager.serialize_into_session { |user| "class:#{user}" }

      manager = build_manager do |config|
        config.serialize_into_session { |user| "manager:#{user}" }
      end

      serializer = proxy_for(manager).session_serializer
      session = {}
      serializer.instance_variable_get(:@env)['rack.session'] = session
      serializer.store("42", :default)

      expect(session["warden.user.default.key"]).to eq("manager:42")
    end

    it "prefers a scope specific serializer over the default serializer of the same Manager" do
      manager = build_manager do |config|
        config.serialize_into_session { |user| "default:#{user}" }
        config.serialize_into_session(:admin) { |user| "admin:#{user}" }
      end

      serializer = proxy_for(manager).session_serializer
      session = {}
      serializer.instance_variable_get(:@env)['rack.session'] = session
      serializer.store("42", :admin)

      expect(session["warden.user.admin.key"]).to eq("admin:42")
    end

    it "prefers the class level scope specific serializer over a Manager default serializer" do
      Warden::Manager.serialize_into_session(:admin) { |user| "class-admin:#{user}" }

      manager = build_manager do |config|
        config.serialize_into_session { |user| "manager-default:#{user}" }
      end

      serializer = proxy_for(manager).session_serializer
      session = {}
      serializer.instance_variable_get(:@env)['rack.session'] = session
      serializer.store("42", :admin)

      expect(session["warden.user.admin.key"]).to eq("class-admin:42")
    end

    it "prefers a Manager scope specific serializer over the class level scope specific serializer" do
      Warden::Manager.serialize_into_session(:admin) { |user| "class-admin:#{user}" }

      manager = build_manager do |config|
        config.serialize_into_session(:admin) { |user| "manager-admin:#{user}" }
      end

      serializer = proxy_for(manager).session_serializer
      session = {}
      serializer.instance_variable_get(:@env)['rack.session'] = session
      serializer.store("42", :admin)

      expect(session["warden.user.admin.key"]).to eq("manager-admin:42")
    end
  end
end
