# frozen_string_literal: true

require "dry/configurable/dsl"

RSpec.describe Dry::Configurable::DSL do
  subject(:dsl) do
    Dry::Configurable::DSL.new
  end

  it "compiles a setting with no options" do
    setting = dsl.setting :user

    expect(setting.name).to be(:user)
  end

  it "compiles a setting with default" do
    setting = dsl.setting :user, default: "root"

    expect(setting.name).to be(:user)
    expect(setting.default).to eq("root")
  end

  it "does not infer a default hash value when non-valid keyword arguments are mixed in with valid keyword arguments" do
    # This test is necessary for behavior specific to Ruby 2.6 and 2.7

    expect { dsl.setting :default_options, foo: "bar", reader: true }.to raise_error ArgumentError, "Invalid options: [:foo]"
  end

  it "compiles a setting with a reader set" do
    setting = dsl.setting(:dsn, default: "sqlite", reader: true)

    expect(setting.name).to be(:dsn)
    expect(setting).to be_reader
  end

  it "compiles a setting with a default string value" do
    setting = dsl.setting(:dsn, default: "sqlite")

    expect(setting.name).to be(:dsn)
    expect(setting.default).to eql("sqlite")
  end

  it "compiles a setting with a default hash value" do
    default = {user: "root", pass: "secret"}

    setting = dsl.setting(:dsn, default: default)

    expect(setting.name).to be(:dsn)
    expect(setting.default).to eql(default)
  end

  it "compiles a setting with a constructor" do
    setting = dsl.setting(:dsn, default: "sqlite", constructor: ->(value) { "jdbc:#{value}" })

    expect(setting.name).to be(:dsn)
    expect(setting.default).to eq("sqlite")
    expect(setting.constructor.("sqlite")).to eq("jdbc:sqlite")
  end

  it "rejects setting names that conflict with Data instance methods" do
    expect { dsl.setting :hash }.to raise_error(
      ArgumentError,
      /:hash is not a valid setting name.*Data instance method.*Config#to_data/
    )

    expect { dsl.setting :members }.to raise_error(
      ArgumentError,
      /:members is not a valid setting name/
    )

    expect { dsl.setting :class }.to raise_error(
      ArgumentError,
      /:class is not a valid setting name/
    )
  end

  it "compiles a nested list of settings" do
    setting =
      dsl.setting(:db) do
        setting(:cred) do
          setting(:user)
          setting(:pass)
        end
      end

    expect(setting.name).to be(:db)

    expect(setting.children.to_a.length).to eq 1
    expect(setting.children.to_a[0].name).to be(:cred)
    expect(setting.children.to_a[0].children.to_a.length).to eq 2
    expect(setting.children.to_a[0].children.to_a[0].name).to be(:user)
    expect(setting.children.to_a[0].children.to_a[1].name).to be(:pass)
  end

  it "compiles a setting with an env name" do
    setting = dsl.setting :dsn, default: "sqlite", env: "DATABASE_URL"

    expect(setting.env).to eql("DATABASE_URL")
  end

  it "normalizes a symbol env name to a string" do
    setting = dsl.setting :dsn, default: "sqlite", env: :DATABASE_URL

    expect(setting.env).to eql("DATABASE_URL")
  end

  it "keeps the env name nil when no env name is given" do
    setting = dsl.setting :dsn, default: "sqlite"

    expect(setting.env).to be(nil)
  end

  it "rejects a non-string non-symbol env name" do
    expect {
      dsl.setting :dsn, default: "sqlite", env: 123
    }.to raise_error(ArgumentError, "env for :dsn must be a String or a Symbol")
  end

  it "rejects an empty env name" do
    expect {
      dsl.setting :dsn, default: "sqlite", env: " "
    }.to raise_error(ArgumentError, "env for :dsn must not be empty")
  end
end
