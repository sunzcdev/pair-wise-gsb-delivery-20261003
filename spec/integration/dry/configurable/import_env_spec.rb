# frozen_string_literal: true

RSpec.describe Dry::Configurable, "#import_env" do
  shared_context "configurable behavior" do
    describe "basic import" do
      before do
        klass.setting :db, default: "sqlite", env: "DATABASE_URL"
        klass.setting :plain, default: "untouched"
      end

      it "imports declared values and reports the keys it used, without values" do
        used = object.config.import_env(
          {"DATABASE_URL" => "postgres", "UNKNOWN" => "ignored"}
        )

        expect(used).to eql(["DATABASE_URL"])
        expect(object.config.db).to eql("postgres")
      end

      it "ignores keys no setting declares" do
        object.config.import_env({"UNKNOWN" => "whatever"})

        expect(object.config.db).to eql("sqlite")
        expect(object.config.plain).to eql("untouched")
      end

      it "accepts symbol keys in the provided hash" do
        used = object.config.import_env({DATABASE_URL: "postgres"})

        expect(used).to eql(["DATABASE_URL"])
        expect(object.config.db).to eql("postgres")
      end

      it "prefers the string key when both string and symbol keys are present" do
        object.config.import_env({"DATABASE_URL" => "from_string", DATABASE_URL: "from_symbol"})

        expect(object.config.db).to eql("from_string")
      end
    end

    describe "missing, empty and nil values" do
      before do
        klass.setting :db, default: "sqlite", env: "DATABASE_URL"
        klass.setting :empty, default: "default", env: "EMPTY"
        klass.setting :nilish, default: "default", env: "NILISH"
      end

      it "leaves the current value untouched when the key is missing" do
        used = object.config.import_env({})

        expect(used).to eql([])
        expect(object.config.db).to eql("sqlite")
      end

      it "does not report a key whose value is missing" do
        object.config.db = "already-set"

        expect(object.config.import_env({"OTHER" => "x"})).to eql([])
        expect(object.config.db).to eql("already-set")
      end

      it "hands empty strings to the constructor and reports the key as used" do
        used = object.config.import_env({"EMPTY" => ""})

        expect(used).to eql(["EMPTY"])
        expect(object.config.empty).to eql("")
      end

      it "passes nil through like any other present value" do
        used = object.config.import_env({"NILISH" => nil})

        expect(used).to eql(["NILISH"])
        expect(object.config.nilish).to be(nil)
      end
    end

    describe "constructors" do
      it "never coerces booleans, numbers or lists on its own" do
        klass.setting :flag, default: true, env: "FLAG"
        klass.setting :list, default: [], env: "LIST"

        object.config.import_env({"FLAG" => "false", "LIST" => "a,b,c"})

        expect(object.config.flag).to eql("false")
        expect(object.config.list).to eql("a,b,c")
      end

      it "runs the declared constructor with the raw string" do
        klass.setting :db, default: "sqlite", env: "DATABASE_URL",
          constructor: -> value { "jdbc:#{value}" }

        object.config.import_env({"DATABASE_URL" => "postgres"})

        expect(object.config.db).to eql("jdbc:postgres")
      end

      it "runs the constructor for empty strings too" do
        klass.setting :db, default: "default", env: "EMPTY",
          constructor: -> value { value.inspect }

        object.config.import_env({"EMPTY" => ""})

        expect(object.config.db).to eql(%q{""})
      end
    end

    describe "nested settings" do
      before do
        klass.setting :db do
          setting :dsn, default: "sqlite", env: "DATABASE_URL"
          setting :pool, default: 5, env: "POOL"
        end
        klass.setting :untracked, default: "x"
      end

      it "imports values into nested settings using their declared names" do
        used = object.config.import_env({"DATABASE_URL" => "postgres", "POOL" => "10"})

        expect(used).to eql(["DATABASE_URL", "POOL"])
        expect(object.config.db.dsn).to eql("postgres")
        expect(object.config.db.pool).to eql("10")
      end

      it "joins prefixes across nested settings that declare names" do
        klass.setting :server do
          setting :host, default: "localhost", env: "WEB_HOST"
        end

        used = object.config.import_env({"SERVER_WEB_HOST" => "0.0.0.0"}, prefix: "SERVER")

        expect(used).to eql(["SERVER_WEB_HOST"])
        expect(object.config.server.host).to eql("0.0.0.0")
      end

      it "does not use setting names of nested settings that declare no env name" do
        klass.setting :deep do
          setting :level, default: 1, env: "LEVEL"
        end

        used = object.config.import_env({"APP_LEVEL" => "2"}, prefix: "APP_")

        expect(used).to eql(["APP_LEVEL"])
        expect(object.config.deep.level).to eql("2")
      end

      it "uses a nested setting's env name as a segment for its children" do
        klass.setting :db, env: "DATABASE" do
          setting :url, default: "sqlite", env: "URL"
        end

        used = object.config.import_env({"DATABASE_URL" => "postgres"})

        expect(used).to eql(["DATABASE_URL"])
        expect(object.config.db.url).to eql("postgres")
      end
    end

    describe "prefix" do
      before do
        klass.setting :host, default: "localhost", env: "HOST"
      end

      it "prepends the prefix joined with an underscore" do
        object.config.import_env({"APP_HOST" => "example.com"}, prefix: :APP)

        expect(object.config.host).to eql("example.com")
      end

      it "does not require a trailing underscore on the prefix" do
        object.config.import_env({"APP_HOST" => "example.com"}, prefix: "APP_")

        expect(object.config.host).to eql("example.com")
      end
    end

    describe "duplicate env mappings" do
      before do
        klass.setting :a, default: 1, env: "SAME_KEY"
        klass.setting :nested do
          setting :b, default: 2, env: "SAME_KEY"
        end
      end

      it "raises before any assignment and names both config paths and the env key" do
        expect {
          object.config.import_env({"SAME_KEY" => "value"})
        }.to raise_error(
          Dry::Configurable::DuplicateEnvKeyError,
          /"SAME_KEY".*a.*nested\.b/
        )

        expect(object.config.a).to be(1)
        expect(object.config.nested.b).to be(2)
      end

      it "detects the conflict even when no environment value is provided" do
        expect { object.config.import_env({}) }.to raise_error(
          Dry::Configurable::DuplicateEnvKeyError
        )
      end

      it "does not materialize nested configs while checking for conflicts" do
        begin
          object.config.import_env({"SAME_KEY" => "value"})
        rescue Dry::Configurable::DuplicateEnvKeyError
          # expected
        end

        expect(object.config.configured?(:nested)).to be(false)
      end
    end

    describe "atomicity" do
      before do
        klass.setting :db do
          setting :dsn, default: "sqlite", env: "DATABASE_URL"
          setting :pool, default: 5, env: "POOL",
            constructor: -> value { Integer(value) }
        end
        klass.setting :other, default: "untouched", env: "OTHER"
      end

      it "leaves no partial update when a nested constructor fails" do
        expect {
          object.config.import_env(
            {"DATABASE_URL" => "postgres", "POOL" => "not-a-number", "OTHER" => "changed"}
          )
        }.to raise_error(Dry::Configurable::EnvImportError) { |error|
          expect(error.message).to include('"POOL"')
          expect(error.message).to include("db.pool")
          expect(error.cause).to be_a(ArgumentError)
        }

        expect(object.config.db.dsn).to eql("sqlite")
        expect(object.config.db.pool).to be(5)
        expect(object.config.other).to eql("untouched")
      end

      it "rolls back assignments made earlier in the same import" do
        klass.setting :broken, default: 0, env: "BROKEN",
          constructor: -> value { raise "boom" }

        expect {
          object.config.import_env({"OTHER" => "changed", "BROKEN" => "x"})
        }.to raise_error(Dry::Configurable::EnvImportError, /"BROKEN".*broken/)

        expect(object.config.other).to eql("untouched")
      end

      it "wraps assignment failures with the env key and the config path" do
        object.finalize!

        expect {
          object.config.import_env({"DATABASE_URL" => "postgres"})
        }.to raise_error(Dry::Configurable::EnvImportError) { |error|
          expect(error.message).to include('"DATABASE_URL"')
          expect(error.message).to include("db.dsn")
          expect(error.cause).to be_a(Dry::Configurable::FrozenConfigError)
        }
      end
    end

    describe "ordinary configuration" do
      before do
        klass.setting :db, default: "sqlite", env: "DATABASE_URL"
        klass.setting :server do
          setting :host, default: "localhost"
        end
      end

      it "keeps plain writers working" do
        object.config.db = "postgres"

        expect(object.config.db).to eql("postgres")
      end

      it "keeps nested update working" do
        object.config.update(server: {host: "example.com"})

        expect(object.config.server.host).to eql("example.com")
      end

      it "can be followed by another import" do
        object.config.import_env({"DATABASE_URL" => "postgres"})
        object.config.import_env({})

        expect(object.config.db).to eql("postgres")
      end
    end
  end

  context "when extended" do
    subject(:object) do
      klass
    end

    let(:klass) do
      Class.new do
        extend Dry::Configurable
      end
    end

    include_context "configurable behavior"
  end

  context "when included" do
    subject(:object) do
      klass.new
    end

    let(:klass) do
      Class.new do
        include Dry::Configurable
      end
    end

    include_context "configurable behavior"
  end
end
