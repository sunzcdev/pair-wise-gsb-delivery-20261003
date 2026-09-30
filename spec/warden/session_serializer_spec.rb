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

  describe "tri-state restore" do
    it "returns a :restored result and keeps the session value when the user is recovered" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_return("USER OBJECT")
      result = @session.restore(:default)
      expect(result).to be_restored
      expect(result.user).to eq("USER OBJECT")
      expect(@env['rack.session']).to have_key("warden.user.default.key")
    end

    it "returns a :missing result when nothing is stored" do
      result = @session.restore(:default)
      expect(result).to be_missing
      expect(result.user).to be_nil
    end

    it "returns a :missing result and deletes the session when the identity no longer exists" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_return(nil)
      result = @session.restore(:default)
      expect(result).to be_missing
      expect(@env['rack.session']).not_to have_key("warden.user.default.key")
    end

    it "returns an :unavailable result and preserves the session value when the store is down" do
      @session.store("user", :default)
      error = Warden::IdentityStoreUnavailable.new("boom", :key => "user")
      allow(@session).to receive(:deserialize).and_raise(error)
      result = @session.restore(:default)
      expect(result).to be_unavailable
      expect(result.error).to eq(error)
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end

    it "keeps fetch returning nil but preserving the session during an outage" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_raise(Warden::IdentityStoreUnavailable)
      expect(@session.fetch(:default)).to be_nil
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end

    it "supports a nil session store with a :missing result" do
      @env['rack.session'] = nil
      expect(@session.restore(:default)).to be_missing
    end

    it "recovers the preserved session once the store is back" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_raise(Warden::IdentityStoreUnavailable)
      expect(@session.restore(:default)).to be_unavailable

      allow(@session).to receive(:deserialize).and_return("USER OBJECT")
      result = @session.restore(:default)
      expect(result).to be_restored
      expect(result.user).to eq("USER OBJECT")
    end

    it "does not rescue unrelated errors" do
      @session.store("user", :default)
      allow(@session).to receive(:deserialize).and_raise(RuntimeError, "real bug")
      expect { @session.restore(:default) }.to raise_error(RuntimeError, "real bug")
      expect(@env['rack.session']['warden.user.default.key']).to eq("user")
    end
  end
end
