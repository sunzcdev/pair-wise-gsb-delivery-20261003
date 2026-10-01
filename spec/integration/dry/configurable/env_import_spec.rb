# frozen_string_literal: true

RSpec.describe Dry::Configurable, "env import" do
  subject(:klass) do
    Class.new do
      extend Dry::Configurable

      setting :dsn, default: "sqlite:memory", env: "DATABASE_DSN"
      setting :retries, default: 3, env: "RETRIES"
      setting :flag, default: false, env: "FLAG"
      setting :hosts, default: [], env: "HOSTS"
      setting :pool_size, default: 5, constructor: -> v { Integer(v) }, env: "POOL_SIZE"
      setting :level, default: "info"

      setting :database do
        setting :user, default: "root", env: "DB_USER"
        setting :password, default: nil, env: "DB_PASSWORD"
        setting :port, default: 5432, constructor: -> v { Integer(v) }, env: "DB_PORT"
      end
    end
  end

  it "imports declared settings from a hash" do
    klass.import_env({"DATABASE_DSN" => "postgres://db", "DB_USER" => "admin"})

    expect(klass.config.dsn).to eql("postgres://db")
    expect(klass.config.database.user).to eql("admin")
  end

  it "works for instance-level configuration" do
    instance_klass = Class.new do
      include Dry::Configurable

      setting :dsn, default: nil, env: "DATABASE_DSN"
    end

    object = instance_klass.new
    object.import_env({"DATABASE_DSN" => "postgres://db"})

    expect(object.config.dsn).to eql("postgres://db")
  end

  it "prepends the given prefix to declared env names" do
    klass.import_env({"APP_DATABASE_DSN" => "postgres://db"}, prefix: "APP_")

    expect(klass.config.dsn).to eql("postgres://db")
  end

  it "keeps current values for missing keys" do
    klass.config.dsn = "custom"

    klass.import_env({"DB_USER" => "admin"})

    expect(klass.config.dsn).to eql("custom")
    expect(klass.config.retries).to be(3)
  end

  it "passes empty strings through the constructor" do
    klass.import_env({"DATABASE_DSN" => ""})

    expect(klass.config.dsn).to eql("")

    expect { klass.import_env({"POOL_SIZE" => ""}) }.to raise_error(
      Dry::Configurable::EnvImportError, /POOL_SIZE/
    )
  end

  it "does not coerce booleans, numbers or comma-separated lists" do
    klass.import_env({"FLAG" => "true", "RETRIES" => "10", "HOSTS" => "a,b,c"})

    expect(klass.config.flag).to eql("true")
    expect(klass.config.retries).to eql("10")
    expect(klass.config.hosts).to eql("a,b,c")
  end

  it "ignores unknown keys in the source" do
    used = klass.import_env({"UNKNOWN" => "value"})

    expect(used).to be_empty
    expect(klass.config.dsn).to eql("sqlite:memory")
  end

  it "leaves settings without an env mapping untouched" do
    klass.import_env({"LEVEL" => "debug", "DATABASE_DSN" => "postgres://db"})

    expect(klass.config.level).to eql("info")
  end

  it "returns the env keys used for the import, without values" do
    used = klass.import_env({
      "DATABASE_DSN" => "postgres://db",
      "DB_USER" => "admin",
      "UNKNOWN" => "value"
    })

    expect(used).to contain_exactly("DATABASE_DSN", "DB_USER")
  end

  it "marks imported settings as configured" do
    klass.import_env({"DATABASE_DSN" => "postgres://db"})

    expect(klass.config.configured?(:dsn)).to be(true)
    expect(klass.config.configured?(:retries)).to be(false)
  end

  context "with conflicting env keys" do
    it "raises before any assignment, naming the conflicting config paths" do
      conflicted = Class.new do
        extend Dry::Configurable

        setting :one, default: "1", env: "SHARED"
        setting :two, default: "2", env: "SHARED"
      end

      expect { conflicted.import_env({"SHARED" => "x"}) }.to raise_error(
        Dry::Configurable::EnvConflictError, /SHARED.*one.*two/
      )

      expect(conflicted.config.one).to eql("1")
      expect(conflicted.config.two).to eql("2")
    end

    it "detects conflicts introduced by the prefix and nested settings" do
      conflicted = Class.new do
        extend Dry::Configurable

        setting :dsn, default: nil, env: "DSN"

        setting :database do
          setting :dsn, default: nil, env: "DSN"
        end
      end

      expect { conflicted.import_env({}, prefix: "APP_") }.to raise_error(
        Dry::Configurable::EnvConflictError, /APP_DSN.*dsn.*database\.dsn/
      )
    end
  end

  context "when a constructor fails" do
    it "raises an error naming the env key and config path, leaving the config untouched" do
      expect {
        klass.import_env({"DATABASE_DSN" => "postgres://db", "POOL_SIZE" => "not-a-number"})
      }.to raise_error(Dry::Configurable::EnvImportError, /POOL_SIZE.*pool_size/)

      expect(klass.config.dsn).to eql("sqlite:memory")
      expect(klass.config.pool_size).to be(5)
    end

    it "exposes the env key and path on the error" do
      klass.import_env({"POOL_SIZE" => "not-a-number"})
    rescue Dry::Configurable::EnvImportError => error
      expect(error.env_key).to eql("POOL_SIZE")
      expect(error.path).to eql("pool_size")
      expect(error.cause).to be_a(ArgumentError)
    end

    it "reports nested config paths and applies no partial updates" do
      expect {
        klass.import_env({"DB_USER" => "admin", "DB_PORT" => "not-a-number"})
      }.to raise_error(Dry::Configurable::EnvImportError, /DB_PORT.*database\.port/)

      expect(klass.config.database.user).to eql("root")
      expect(klass.config.database.port).to be(5432)
    end
  end

  it "raises when the config is frozen, without changing values" do
    klass.finalize!

    expect { klass.import_env({"DATABASE_DSN" => "postgres://db"}) }.to raise_error(
      Dry::Configurable::FrozenConfigError
    )

    expect(klass.config.dsn).to eql("sqlite:memory")
  end

  it "does not allow env: on settings with nested settings" do
    expect {
      klass.setting(:db, env: "DB") { setting :url }
    }.to raise_error(ArgumentError, /env:/)
  end

  it "keeps #update working as before" do
    klass.config.update(dsn: "mysql://db", database: {user: "admin"})

    expect(klass.config.dsn).to eql("mysql://db")
    expect(klass.config.database.user).to eql("admin")
  end
end
