# encoding: utf-8
# frozen_string_literal: true
RSpec.describe Warden::Manager do
  before(:each) do
    @env = env_with_params
    @env['rack.session'] ||= {}
    Warden::Manager.serialize_from_session { |k| k }
    Warden::Manager.serialize_into_session { |u| u }
    [:admin_serialize, :admin_deserialize].each do |name|
      Warden::SessionSerializer.send(:remove_method, name) if Warden::SessionSerializer.method_defined?(name)
    end
  end

  after(:each) do
    Warden::Manager.serialize_from_session { |k| k }
    Warden::Manager.serialize_into_session { |u| u }
    [:admin_serialize, :admin_deserialize].each do |name|
      Warden::SessionSerializer.send(:remove_method, name) if Warden::SessionSerializer.method_defined?(name)
    end
  end

  def serializer_respond_to?(name)
    Warden::SessionSerializer.new(@env).respond_to? name
  end

  it "responds to :serialize" do
    expect(serializer_respond_to?(:serialize)).to be(true)
  end

  it "responds to :deserialize" do
    expect(serializer_respond_to?(:deserialize)).to be(true)
  end

  it "responds to {scope}_deserialize when Manager.serialize_from_session is called with a scope" do
    Warden::Manager.serialize_from_session(:admin) { |n| n }
    expect(serializer_respond_to?(:admin_deserialize)).to be(true)
  end

  it "responds to {scope}_serialize when Manager.serialize_into_session is called with a scope" do
    Warden::Manager.serialize_into_session(:admin) { |n| n }
    expect(serializer_respond_to?(:admin_serialize)).to be(true)
  end

  it "executes serialize when no {scope}_serialize is present" do
    serialized_object = nil
    Warden::Manager.serialize_into_session do |user|
      serialized_object = user
      user
    end

    Warden::SessionSerializer.new(@env).store("user", :admin)
    expect(serialized_object).to eq("user")
  end

  it "does not have a {scope}_serialize by default" do
    expect(serializer_respond_to?(:admin_serialize)).to be(false)
  end

  it "executes {scope}_serialize when storing a scope" do
    serialized_object = nil
    Warden::Manager.serialize_into_session(:admin) do |user|
      serialized_object = user
      user
    end

    Warden::SessionSerializer.new(@env).store("user", :admin)
    expect(serialized_object).to eq("user")
  end

  it "executes {scope}_deserialize when fetching a scope" do
    serialized_object = nil
    Warden::Manager.serialize_from_session(:admin) do |key|
      serialized_object = key
      key
    end

    serializer = Warden::SessionSerializer.new(@env)
    @env['rack.session'][serializer.key_for(:admin)] = "test"
    serializer.fetch(:admin)

    expect(serialized_object).to eq("test")
  end

  it "executes deserialize when no {scope}_deserialize is present" do
    serialized_object = nil
    Warden::Manager.serialize_from_session do |key|
      serialized_object = key
      key
    end

    serializer = Warden::SessionSerializer.new(@env)
    @env['rack.session'][serializer.key_for(:admin)] = "test"
    serializer.fetch(:admin)

    expect(serialized_object).to eq("test")
  end
end
