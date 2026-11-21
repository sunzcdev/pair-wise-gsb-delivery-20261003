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

  it "records the full dotted path of each nested setting" do
    setting =
      dsl.setting(:db) do
        setting(:cred) do
          setting(:user)
          setting(:pass)
        end
      end

    cred = setting.children.to_a[0]

    expect(setting.path).to eq([:db])
    expect(cred.path).to eq([:db, :cred])
    expect(cred.children.to_a.map(&:path)).to eq([[:db, :cred, :user], [:db, :cred, :pass]])
    expect(cred.children.to_a.map(&:full_name)).to eq(["db.cred.user", "db.cred.pass"])
  end

  it "does not register a setting that fails to compile" do
    expect { dsl.setting(:db) { setting(:"not valid") } }.to raise_error(
      ArgumentError, /not a valid/
    )

    expect(dsl.ast).to be_empty
  end

  it "does not register a setting when the nested DSL block raises" do
    expect { dsl.setting(:db) { raise "boom" } }.to raise_error("boom")

    expect(dsl.ast).to be_empty
  end

  context "with a configurable owner" do
    let(:owner) do
      Class.new do
        extend Dry::Configurable
      end
    end

    subject(:dsl) do
      owner.__config_dsl__
    end

    it "attributes compiled settings to the owner" do
      setting =
        dsl.setting(:db) do
          setting(:cred) do
            setting(:user)
          end
        end

      user = setting.children.to_a[0].children.to_a[0]

      expect(user.owner).to be(owner)
      expect(user.path).to eq([:db, :cred, :user])
      expect(user.description).to include("db.cred.user")
    end
  end
end
