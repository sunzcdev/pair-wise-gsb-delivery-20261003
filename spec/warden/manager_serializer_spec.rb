# encoding: utf-8
# frozen_string_literal: true
RSpec.describe "Manager session serialization" do
  failure_app = Warden::Spec::Helpers::FAILURE_APP

  # Runs one request through the manager, exposing the resulting proxy.
  def request(manager, session = {})
    env = env_with_params("/", {}).merge("rack.session" => session)
    manager.call(env)
    env["warden"]
  end

  def build_app(&block)
    Warden::Manager.new(lambda { |_env| [200, {}, []] }) do |config|
      config.failure_app = failure_app
      block.call(config)
    end
  end

  # Two applications mounted in the same process: both authenticate the
  # :default scope but write the identity into the session differently.
  let(:app_one) do
    build_app do |config|
      config.serialize_into_session { |user| "one:#{user}" }
      config.serialize_from_session { |key| key.sub(/^one:/, "") }
    end
  end

  let(:app_two) do
    build_app do |config|
      config.serialize_into_session { |user| ["two", user] }
      config.serialize_from_session { |key| key.is_a?(Array) ? key.last : nil }
    end
  end

  it "does not redefine methods on the shared SessionSerializer class" do
    defined_on_base = Warden::SessionSerializer.instance_methods(false).map(&:to_s).sort
    app_one
    app_two
    expect(Warden::SessionSerializer.instance_methods(false).map(&:to_s).sort).to eq(defined_on_base)
  end

  it "writes the same raw id using different session representations" do
    session = {}
    request(app_one, session).set_user("42")
    expect(session["warden.user.default.key"]).to eq("one:42")

    session = {}
    request(app_two, session).set_user("42")
    expect(session["warden.user.default.key"]).to eq(["two", "42"])
  end

  it "restores each application only from its own representation" do
    session_one = { "warden.user.default.key" => "one:7" }
    expect(request(app_one, session_one).user(:default)).to eq("7")

    session_two = { "warden.user.default.key" => ["two", 9] }
    expect(request(app_two, session_two).user(:default)).to eq(9)
  end

  it "does not cross over when one application reads the other's session" do
    session = {}
    request(app_one, session).set_user("42")
    expect(session["warden.user.default.key"]).to eq("one:42")

    # Application two receives the exact representation written by app one.
    proxy = request(app_two, session)
    expect(proxy.user(:default)).to be_nil
    # The foreign, unreadable representation is discarded from the session.
    expect(session).not_to have_key("warden.user.default.key")
  end

  it "interleaves requests of both applications without state leaking" do
    session = {}
    request(app_one, session).set_user("42")
    expect(session["warden.user.default.key"]).to eq("one:42")

    request(app_two, session).user(:default)
    expect(session).not_to have_key("warden.user.default.key")

    expect(request(app_one, session).user(:default)).to be_nil
    request(app_one, session).set_user("42")
    expect(session["warden.user.default.key"]).to eq("one:42")
  end

  describe "fallback to class level settings" do
    after do
      Warden::Manager.serialize_from_session { |key| key }
      Warden::Manager.serialize_into_session { |user| user }
      Warden::SessionSerializer.send(:remove_method, :admin_serialize) rescue nil
      Warden::SessionSerializer.send(:remove_method, :admin_deserialize) rescue nil
    end

    it "uses the class level default serializers when the Manager defines none" do
      Warden::Manager.serialize_into_session { |user| "global:#{user}" }
      Warden::Manager.serialize_from_session { |key| key.to_s.sub(/^global:/, "") }

      manager = build_app { }
      session = {}
      request(manager, session).set_user("3")
      expect(session["warden.user.default.key"]).to eq("global:3")

      expect(request(manager, session).user(:default)).to eq("3")
    end

    it "lets a Manager default override the class level default" do
      Warden::Manager.serialize_into_session { |user| "global:#{user}" }
      Warden::Manager.serialize_from_session { |key| key.to_s.sub(/^global:/, "") }

      manager = build_app do |config|
        config.serialize_into_session { |user| "local:#{user}" }
        config.serialize_from_session { |key| key.to_s.sub(/^local:/, "") }
      end

      session = {}
      request(manager, session).set_user("5")
      expect(session["warden.user.default.key"]).to eq("local:5")
      expect(request(manager, session).user(:default)).to eq("5")
    end

    it "prefers the Manager scope serializer over the Manager default" do
      manager = build_app do |config|
        config.serialize_into_session { |user| "default:#{user}" }
        config.serialize_from_session { |key| key.to_s.sub(/^default:/, "") }
        config.serialize_into_session(:admin) { |user| "admin:#{user}" }
        config.serialize_from_session(:admin) { |key| key.to_s.sub(/^admin:/, "") }
      end

      session = {}
      proxy = request(manager, session)
      proxy.set_user("1", :scope => :admin)
      proxy.set_user("2", :scope => :default)
      expect(session["warden.user.admin.key"]).to eq("admin:1")
      expect(session["warden.user.default.key"]).to eq("default:2")

      proxy = request(manager, session)
      expect(proxy.user(:admin)).to eq("1")
      expect(proxy.user(:default)).to eq("2")
    end

    it "falls back to the class level scope serializer for an unconfigured scope" do
      Warden::Manager.serialize_into_session(:admin) { |user| "global-admin:#{user}" }
      Warden::Manager.serialize_from_session(:admin) { |key| key.to_s.sub(/^global-admin:/, "") }

      manager = build_app do |config|
        config.serialize_into_session { |user| "local:#{user}" }
        config.serialize_from_session { |key| key.to_s.sub(/^local:/, "") }
      end

      session = {}
      request(manager, session).set_user("8", :scope => :admin)
      expect(session["warden.user.admin.key"]).to eq("global-admin:8")
      expect(request(manager, session).user(:admin)).to eq("8")
    end
  end

  describe "Proxy protocol snapshot" do
    it "keeps a running Proxy unchanged when another Manager is configured" do
      session = {}
      proxy = request(app_one, session)

      build_app do |config|
        config.serialize_into_session { |user| "two:#{user}" }
        config.serialize_from_session { |key| key.to_s.sub(/^two:/, "") }
      end

      proxy.set_user("42")
      expect(session["warden.user.default.key"]).to eq("one:42")
    end

    it "keeps a captured Proxy stable when its own Manager is reconfigured" do
      manager = build_app do |config|
        config.serialize_into_session { |user| "before:#{user}" }
      end

      session = {}
      proxy = request(manager, session)

      manager.serialize_into_session { |user| "after:#{user}" }

      proxy.set_user("1")
      expect(session["warden.user.default.key"]).to eq("before:1")
    end

    it "uses the new configuration for subsequent requests" do
      manager = build_app do |config|
        config.serialize_into_session { |user| "before:#{user}" }
        config.serialize_from_session { |key| key.to_s.sub(/^before:/, "") }
      end

      session = {}
      request(manager, session).set_user("1")
      expect(session["warden.user.default.key"]).to eq("before:1")

      manager.serialize_into_session { |user| "after:#{user}" }
      manager.serialize_from_session { |key| key.to_s.sub(/^after:/, "") }

      session = {}
      request(manager, session).set_user("2")
      expect(session["warden.user.default.key"]).to eq("after:2")
    end
  end
end
