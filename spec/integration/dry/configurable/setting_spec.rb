# frozen_string_literal: true

require "pathname"

RSpec.describe Dry::Configurable, ".setting" do
  shared_context "configurable behavior" do
    context "without a default value" do
      before do
        klass.setting :db
      end

      it "sets nil as the default" do
        expect(object.config.db).to be(nil)
      end

      it "allows configuring a setting using a writer" do
        object.config.db = "sqlite"

        expect(object.config.db).to eql("sqlite")
      end

      it "allows configuring a setting using a square-bracket writer" do
        object.config[:db] = "sqlite"

        expect(object.config.db).to eql("sqlite")

        object.config[:db] = "mariadb"

        expect(object.config.db).to eql("mariadb")
      end
    end

    it "raises when invalid options are passed" do
      expect {
        klass.setting :db_config, user: "root", password: "", reader: true
      }.to raise_error(
        ArgumentError, "Invalid options: [:user, :password]"
      )
    end

    it "stores setting name as symbol" do
      klass.setting "db", default: "sqlite"

      expect(object.config.values.keys).to include(:db)
    end

    context "with a default value" do
      context "string" do
        before do
          klass.setting :db, default: "sqlite"
        end

        it "presets the default value" do
          expect(object.config.db).to eql("sqlite")
        end
      end

      context "hash" do
        it "returns the default value" do
          klass.setting :db_config, default: {user: "root", password: ""}

          expect(object.config.db_config).to eql(user: "root", password: "")
        end

        it "copies the original hash object" do
          hash = {user: "root", password: ""}

          klass.setting :db_config, default: hash

          expect(object.config.db_config).to_not be(hash)
          expect(object.config.db_config).to eql(hash)
        end
      end
    end

    context "with nested settings" do
      before do
        klass.setting :db do
          setting :type, default: "sqlite"
          setting :cred do
            setting :user
            setting :pass
          end
        end
      end

      it "nests values in the config" do
        expect(object.config.db.type).to eql("sqlite")
        expect(object.config.db.cred.user).to be(nil)
        expect(object.config.db.cred.pass).to be(nil)

        object.config.db.cred.user = "root"
        object.config.db.cred.pass = "secret"

        expect(object.config.db.cred.user).to eql("root")
        expect(object.config.db.cred.pass).to eql("secret")
      end
    end

    context "with a value constructor" do
      it "constructs the value with nil default" do
        klass.setting(:path, default: nil, constructor: ->(value) { "test:#{value || "fallback"}" })

        expect(object.config.path).to eql("test:fallback")

        object.configure do |config|
          config.path = "foo"
        end

        expect(object.config.path).to eql("test:foo")
      end

      it "constructs the value with undefined default" do
        klass.setting(:path, constructor: ->(value) { "test:#{value || "fallback"}" })

        expect(object.config.path).to eql("test:fallback")
      end

      it "constructs the value with non-nil default" do
        klass.setting(:path, default: "test", constructor: ->(value) { Pathname(value) })

        expect(object.config.path).to eql(Pathname("test"))
      end

      it "raises constructor errors immediately" do
        klass.setting(:failable, constructor: ->(value) { value&.to_sym })

        expect {
          object.config.failable = 12
        }.to raise_error(NoMethodError, /undefined method (`|')to_sym'/)
      end

      it "names the field and declaring class when the constructor fails" do
        failable_klass = Class.new do
          extend Dry::Configurable

          class << self
            def name = "FailablePlugin"
          end

          setting :connection do
            setting :timeout, default: "10", constructor: ->(value) { Integer(value) }
          end
        end

        expect { failable_klass.config.connection.timeout = "soon" }.to raise_error(
          ArgumentError, /setting "connection\.timeout" for FailablePlugin .*soon/
        )

        expect { failable_klass.config.update(connection: {timeout: "later"}) }.to raise_error(
          ArgumentError, /setting "connection\.timeout" for FailablePlugin .*later/
        )
      end

      it "leaves the setting untouched when its constructor fails" do
        klass.setting(:timeout, default: "10", constructor: ->(value) { Integer(value) })

        expect { object.config.timeout = "soon" }.to raise_error(ArgumentError)

        expect(object.config.timeout).to be(10)
        expect(object.config.configured?(:timeout)).to be(false)

        object.config.timeout = "11"
        expect(object.config.timeout).to be(11)
        expect(object.config.configured?(:timeout)).to be(true)
      end
    end

    context "with reader: true" do
      it "defines a reader shortcut when there is no default" do
        klass.setting :db, reader: true

        expect(object.db).to be(nil)
      end

      it "defines a reader shortcut when there is default" do
        klass.setting :db, default: "sqlite", reader: true

        expect(object.db).to eql("sqlite")
      end
    end

    context "with a ruby keyword" do
      before do
        klass.setting :if, default: true
      end

      it "works" do
        expect(object.config.if).to be(true)
      end
    end

    context "with :settings as a setting name" do
      before do
        klass.setting :settings, default: true
      end

      it "works" do
        expect(object.config.settings).to be(true)
      end
    end

    it "rejects invalid names" do
      %i[foo? bar! d'oh 7 {} - ==].each do |name|
        expect { klass.setting name }.to raise_error(ArgumentError, /not a valid/)
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

    specify "settings defined after accessing config are still available in the config" do
      klass.setting :before, default: "defined before"
      klass.config
      klass.setting :after, default: "defined after"

      expect(klass.config.before).to eq "defined before"
      expect(klass.config.after).to eq "defined after"
    end

    context "with a subclass" do
      let(:subclass) do
        Class.new(klass)
      end

      it "maintains a value mutated in the superclass in the subclass config" do
        klass.setting :db do
          setting :ports, default: Set[123]
        end

        klass.config.db.ports << 312

        subclass = Class.new(klass)

        expect(subclass.config.db.ports).to eql(Set[123, 312])
      end

      it "allows defining more settings in the subclass" do
        klass.setting :db, default: "sqlite"

        subclass.setting :username, default: "root"
        subclass.setting :password

        subclass.config.password = "secret"

        expect(subclass.config.db).to eql("sqlite")
        expect(subclass.config.username).to eql("root")
        expect(subclass.config.password).to eql("secret")

        expect(klass.config).not_to respond_to(:username)
        expect(klass.config).not_to respond_to(:password)
      end

      it "adding parent setting does not affect child" do
        klass.setting :db, default: "sqlite"

        expect(subclass.settings.map(&:name)).to eql([:db])

        klass.setting :other

        expect(subclass.settings.map(&:name)).to eql([:db])
      end

      specify "configuring the parent before subclassing copies the config to the child" do
        klass.setting :db

        object.config.db = "mariadb"

        expect(subclass.config.db).to eql("mariadb")
      end

      specify "configuring the parent after subclassing does not copy the config to the child" do
        klass.setting :db

        subclass = Class.new(klass)

        object.config.db = "mariadb"

        expect(subclass.config.db).to be nil
      end

      it "not configured parent does not set child config" do
        klass.setting :db

        expect(subclass.config.db).to be(nil)
      end

      it "changing child does not affect parent" do
        klass.setting :db, default: "sqlite"

        klass.setting :nested do
          setting :test, default: "hello"
        end

        subclass.configure do |config|
          config.db = "postgresql"
          config.nested.test = "woah!"
        end

        expect(klass.settings.map(&:name)).to eq([:db, :nested])
        expect(object.config.db).to eql("sqlite")
        expect(object.config.db).to eql("sqlite")
        expect(object.config.nested.test).to eql("hello")

        expect(subclass.settings.map(&:name)).to eq([:db, :nested])
        expect(subclass.config.db).to eql("postgresql")
        expect(subclass.config.nested.test).to eql("woah!")
      end

      it "inherits readers from parent" do
        klass.setting :db, default: "sqlite", reader: true

        expect(subclass.db).to eql("sqlite")
      end

      it "defines a reader shortcut for nested config" do
        klass.setting :dsn, reader: true do
          setting :pool, default: 5
        end

        expect(klass.dsn.pool).to be(5)
      end

      context "with a custom config class" do
        it "uses the custom config class" do
          Test::Config = Class.new(Dry::Configurable::Config)

          klass = Class.new do
            extend Dry::Configurable(config_class: Test::Config)

            setting :dsn, reader: true do
              setting :pool, default: 5
            end
          end

          expect(klass.config).to be_instance_of(Test::Config)
          expect(klass.config.dsn).to be_instance_of(Test::Config)
          expect(klass.dsn.pool).to be(5)
        end

        it "uses the custom config class in nested settings only" do
          Test::Config = Class.new(Dry::Configurable::Config)

          klass = Class.new do
            extend Dry::Configurable

            setting :database do
              setting :dsn, config_class: Test::Config, reader: true do
                setting :pool, default: 5
              end
            end
          end

          expect(klass.config).to be_instance_of(Dry::Configurable::Config)
          expect(klass.config.database).to be_instance_of(Dry::Configurable::Config)
          expect(klass.config.database.dsn).to be_instance_of(Test::Config)
          expect(klass.config.database.dsn.pool).to be(5)
        end

        it "applies the custom config class to every nested level below it" do
          Test::Config = Class.new(Dry::Configurable::Config)

          klass = Class.new do
            extend Dry::Configurable

            setting :database do
              setting :dsn, config_class: Test::Config do
                setting :pool do
                  setting :size, default: 5
                end
              end
            end
          end

          expect(klass.config.database.dsn).to be_instance_of(Test::Config)
          expect(klass.config.database.dsn.pool).to be_instance_of(Test::Config)
          expect(klass.config.database.dsn.pool.size).to be(5)
        end

        it "updates and converts nested settings that override the parent config class" do
          Test::Config = Class.new(Dry::Configurable::Config)

          klass = Class.new do
            extend Dry::Configurable

            setting :database do
              setting :dsn, config_class: Test::Config do
                setting :pool, default: 5
              end
            end
          end

          klass.config.update(database: {dsn: {pool: 9}})

          expect(klass.config.database.dsn.pool).to be(9)
          expect(klass.config.to_h).to eq(database: {dsn: {pool: 9}})

          klass.finalize!

          expect(klass.config.database.dsn).to be_frozen
          expect(klass.config.to_data.database.dsn.pool).to be(9)
        end
      end
    end

    context "with two plugins sharing the same nested setting DSL" do
      let(:shared_dsl) do
        proc do
          setting :throttle, reader: true do
            setting :limit, default: "10", constructor: ->(value) { Integer(value) }
            setting :burst, default: "20", constructor: ->(value) { Integer(value) }
          end
        end
      end

      before do
        Test::PluginConfig = Class.new(Dry::Configurable::Config)

        shared = shared_dsl

        Test::Alpha = Class.new do
          extend Dry::Configurable(config_class: Test::PluginConfig)
          class_exec(&shared)
        end

        Test::Beta = Class.new do
          extend Dry::Configurable
          class_exec(&shared)
        end
      end

      it "builds each plugin's nested config with its own config class" do
        expect(Test::Alpha.config).to be_instance_of(Test::PluginConfig)
        expect(Test::Alpha.config.throttle).to be_instance_of(Test::PluginConfig)

        expect(Test::Beta.config).to be_instance_of(Dry::Configurable::Config)
        expect(Test::Beta.config.throttle).to be_instance_of(Dry::Configurable::Config)
      end

      it "runs each plugin's own constructors for the same field" do
        Test::Alpha.setting :probe, default: "1", constructor: ->(value) { [:alpha, Integer(value)] }
        Test::Beta.setting :probe, default: "1", constructor: ->(value) { [:beta, Integer(value)] }

        expect(Test::Alpha.config.probe).to eq([:alpha, 1])
        expect(Test::Beta.config.probe).to eq([:beta, 1])
      end

      it "names the plugin and field when a nested constructor fails" do
        expect { Test::Alpha.throttle.limit = "not-a-number" }.to raise_error(
          ArgumentError, /setting "throttle\.limit" for Test::Alpha .*not-a-number/
        )

        expect { Test::Beta.config.throttle.burst = Object.new }.to raise_error(
          TypeError, /setting "throttle\.burst" for Test::Beta /
        )
      end

      it "leaves the other plugin configurable after one plugin's conversion fails" do
        expect { Test::Alpha.throttle.limit = "not-a-number" }.to raise_error(ArgumentError)

        # The failing plugin is unchanged and remains configurable
        expect(Test::Alpha.throttle.limit).to be(10)
        expect(Test::Alpha.config.throttle.configured?(:limit)).to be(false)
        Test::Alpha.throttle.limit = "11"
        expect(Test::Alpha.throttle.limit).to be(11)

        # And the other plugin was never affected
        expect(Test::Beta.throttle.limit).to be(10)
        Test::Beta.throttle.burst = "25"
        expect(Test::Beta.throttle.burst).to be(25)
      end

      it "keeps config values and conversion failures separate per plugin" do
        Test::Alpha.config.update(throttle: {limit: "30"})

        expect(Test::Alpha.config.to_h).to eq(throttle: {limit: 30, burst: 20})
        expect(Test::Beta.config.to_h).to eq(throttle: {limit: 10, burst: 20})
      end

      it "does not register a setting whose nested definition fails" do
        expect {
          Test::Beta.setting :broken do
            setting :ok, default: true
            setting :"not valid", default: false
          end
        }.to raise_error(ArgumentError, /not a valid/)

        expect(Test::Beta.settings.map(&:name)).to eq([:throttle])
        expect(Test::Beta.config).not_to respond_to(:broken)

        # The plugin that shares the DSL is unaffected
        expect(Test::Alpha.throttle.limit).to be(10)
        expect(Test::Alpha.config.throttle).to be_instance_of(Test::PluginConfig)
      end
    end
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

    it "creates config detached from the class settings" do
      klass.setting :db, default: "sqlite"

      object.config.db = "mariadb"

      expect(object.config.db).to eql("mariadb")
      expect(klass.new.config.db).to eql("sqlite")
    end

    it "exposes `config` only at the instance-level" do
      expect(klass).to_not respond_to(:config)
    end

    it "exposes `configure` only at the instance-level" do
      expect(klass).to_not respond_to(:configure)
    end

    it "defines a constructor that sets the config" do
      klass.setting :db, default: "sqlite"

      expect(object.config.db).to eql("sqlite")
    end

    it "creates distinct setting values across instances" do
      klass.setting(:path, default: "test", constructor: ->(m) { Pathname(m) })

      new_object = klass.new

      expect(object.config.path).to eq Pathname("test")
      expect(new_object.config.path).to eq Pathname("test")
      expect(object.config.path).not_to be(new_object.config.path)
    end

    it "makes settings defined after instantiation available" do
      klass.setting :before, default: "defined before"

      object_1 = klass.new

      klass.setting :after, default: "defined after"

      object_2 = klass.new

      expect(object_1.config.before).to eq "defined before"
      expect(object_1.config.after).to eq "defined after"

      expect(object_2.config.before).to eq "defined before"
      expect(object_2.config.after).to eq "defined after"
    end

    it "is frozen when finalized" do
      object.finalize!
      object.finalize! # second call becomes a no op

      expect(object).to be_frozen
      expect(object.config).to be_frozen
    end

    shared_examples "copying" do
      before do
        klass.setting :env

        klass.setting :db do
          setting :user, default: "root".dup
          setting :pass, default: "secret"
        end
      end

      it "can be copied" do
        clone = object.clone

        expect(object.config.env).to be(nil)
        expect(clone.config.env).to be(nil)

        expect(object.config.db.user).to eql("root")
        expect(clone.config.db.user).to eql("root")

        object.config.env = "production"
        object.config.db.user = "jane"

        expect(object.config.env).to eql("production")

        expect(object.config.db.user).to eql("jane")
        expect(clone.config.db.user).to eql("root")

        expect(clone.config.db.pass).to eql(object.config.db.pass)
      end
    end

    include_examples "copying" do
      it "stays frozen when cloning" do
        expect(object.finalize!.clone).to be_frozen
      end

      it "stays unfrozen when duping" do
        expect(object.finalize!.dup).to_not be_frozen
      end
    end

    it "can be configured" do
      klass.setting :db, default: "sqlite"

      object.configure do |config|
        config.db = "mariadb"
      end

      expect(object.config.db).to eql("mariadb")
    end

    it "defines a reader shortcut for nested config" do
      klass.setting :dsn, reader: true do
        setting :pool, default: 5
      end

      expect(object.dsn.pool).to be(5)
    end

    context "with a custom config class" do
      it "uses the custom config class" do
        Test::Config = Class.new(Dry::Configurable::Config)

        klass = Class.new do
          include Dry::Configurable(config_class: Test::Config)

          setting :dsn, reader: true do
            setting :pool, default: 5
          end
        end

        object = klass.new

        expect(object.config).to be_instance_of(Test::Config)
        expect(object.dsn.pool).to be(5)
      end
    end

    context "Test Interface" do
      describe "reset_config" do
        it "resets configuration to default values" do
          klass.setting :dsn, default: nil

          klass.setting :pool do
            setting :size, default: nil
          end

          object.enable_test_interface

          object.config.dsn = "sqlite:memory"
          object.config.pool.size = 5

          object.reset_config

          expect(object.config.dsn).to be_nil
          expect(object.config.pool.size).to be_nil
        end
      end
    end
  end
end
