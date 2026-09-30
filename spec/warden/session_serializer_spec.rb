# encoding: utf-8
# frozen_string_literal: true
RSpec.describe Warden::SessionSerializer do
  before(:each) do
    @env = env_with_params
    @env['rack.session'] ||= {}
    @session = Warden::SessionSerializer.new(@env)
  end

  it "should store data for the default scope" do
    @session.store("user", :default)
    expect(@env['rack.session']).to eq({ "warden.user.default.key"=>"user" })
  end

  it "should check if a data is stored or not" do
    expect(@session).not_to be_stored(:default)
    @session.store("user", :default)
    expect(@session).to be_stored(:default)
  end

  it "should load an user from store" do
    expect(@session.fetch(:default)).to be_nil
    @session.store("user", :default)
    expect(@session.fetch(:default)).to eq("user")
  end

  it "should store data based on the scope" do
    @session.store("user", :default)
    expect(@session.fetch(:default)).to eq("user")
    expect(@session.fetch(:another)).to be_nil
  end

  it "should delete data from store" do
    @session.store("user", :default)
    expect(@session.fetch(:default)).to eq("user")
    @session.delete(:default)
    expect(@session.fetch(:default)).to be_nil
  end

  it "should delete information from store if user cannot be retrieved" do
    @session.store("user", :default)
    expect(@env['rack.session']).to have_key("warden.user.default.key")
    allow(@session).to receive(:deserialize).and_return(nil)
    @session.fetch(:default)
    expect(@env['rack.session']).not_to have_key("warden.user.default.key")
  end

  it "should support a nil session store" do
    @env['rack.session'] = nil
    expect(@session.fetch(:default)).to be_nil
  end

  describe "#restore" do
    it "reports :not_found and returns no user when nothing is stored" do
      result = @session.restore(:default)
      expect(result).to be_not_found
      expect(result.status).to eq(:not_found)
      expect(result.user).to be_nil
      expect(result).not_to be_restored
      expect(result).not_to be_unavailable
    end

    it "reports :restored with the user when deserialization succeeds" do
      @session.store("user", :default)
      result = @session.restore(:default)
      expect(result).to be_restored
      expect(result.user).to eq("user")
    end

    it "reports :not_found and removes the session value when the identity is gone" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_return(nil)

      result = @session.restore(:default)
      expect(result).to be_not_found
      expect(@env['rack.session']).not_to have_key("warden.user.default.key")
    end

    it "reports :unavailable and preserves the session value when the store raises" do
      @session.store("user", :default)
      error = Warden::IdentityStoreTemporarilyUnavailable.new("connection refused")
      allow(@session).to receive(:deserialize).and_raise(error)

      result = @session.restore(:default)
      expect(result).to be_unavailable
      expect(result.error).to equal(error)
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end

    it "keeps the original session value untouched across repeated unavailable attempts" do
      @session.store("user", :default)
      calls = 0
      allow(@session).to receive(:deserialize) do
        calls += 1
        raise Warden::IdentityStoreTemporarilyUnavailable, "boom"
      end

      3.times do
        result = @session.restore(:default)
        expect(result).to be_unavailable
        expect(@session.stored?(:default)).to be(true)
      end
      expect(calls).to eq(3)
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end

    it "restores normally once the identity store recovers after a failure phase" do
      @session.store("user", :default)
      unavailable = [true]
      allow(@session).to receive(:deserialize) do |key|
        raise Warden::IdentityStoreTemporarilyUnavailable, "down" if unavailable[0]
        key
      end

      expect(@session.restore(:default)).to be_unavailable
      expect(@session.stored?(:default)).to be(true)

      unavailable[0] = false
      result = @session.restore(:default)
      expect(result).to be_restored
      expect(result.user).to eq("user")
    end

    it "does not rescue unrelated errors" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_raise(RuntimeError, "kaboom")
      expect { @session.restore(:default) }.to raise_error(RuntimeError, "kaboom")
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end
  end

  describe "#fetch compatibility" do
    it "returns the restored user" do
      @session.store("user", :default)
      expect(@session.fetch(:default)).to eq("user")
    end

    it "returns nil and deletes the session when the identity is gone" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_return(nil)
      expect(@session.fetch(:default)).to be_nil
      expect(@env['rack.session']).not_to have_key("warden.user.default.key")
    end

    it "re-raises instead of treating an unavailable store as a deleted identity" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_raise(Warden::IdentityStoreTemporarilyUnavailable, "down")
      expect { @session.fetch(:default) }.to raise_error(Warden::IdentityStoreTemporarilyUnavailable)
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end
  end
end
