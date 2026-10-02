# frozen_string_literal: true

require "pathname"
require "set"

RSpec.describe Dry::Configurable::Config do
  subject(:config) do
    klass.config
  end

  let(:klass) do
    Class.new do
      extend Dry::Configurable
    end
  end

  describe "#configured?" do
    it "returns false if the key has not been accessed" do
      klass.setting :db
      expect(klass.config.configured?(:db)).to be false
    end

    it "returns true if the key has been assigned" do
      klass.setting :db
      klass.config.db = "sqlite"
      expect(klass.config.configured?(:db)).to be true
    end

    it "returns true when the key was assigned in a superclass" do
      klass.setting :db
      klass.config.db = "sqlite"
      subclass = Class.new(klass)
      expect(subclass.config.configured?(:db)).to be true
    end

    describe "assignable values" do
      it "returns false if the key has been read" do
        klass.setting :db
        klass.config.db
        expect(klass.config.configured?(:db)).to be false
      end

      it "returns false when the key was read in a superclass" do
        klass.setting :db
        klass.config.db
        subclass = Class.new(klass)
        expect(subclass.config.configured?(:db)).to be false
      end
    end

    describe "cloneable values" do
      it "returns false if the key has been read only" do
        klass.setting :db, cloneable: true, default: []
        klass.config.db
        expect(klass.config.configured?(:db)).to be false
      end

      it "returns false if the key was read in a superclass" do
        klass.setting :db, cloneable: true, default: []
        klass.config.db
        subclass = Class.new(klass)
        expect(subclass.config.configured?(:db)).to be false
      end

      it "returns true if the value has been mutated" do
        klass.setting :db, cloneable: true, default: []
        klass.config.db << "sqlite"
        expect(klass.config.configured?(:db)).to be true
      end

      it "returns true if the value was mutated in a superclass" do
        klass.setting :db, cloneable: true, default: []
        klass.config.db << "sqlite"
        subclass = Class.new(klass)
        expect(subclass.config.configured?(:db)).to be true
      end
    end
  end

  describe "#update" do
    it "sets new config values in a flat config" do
      klass.setting :db

      config = klass.config.update(db: "sqlite")

      expect(klass.config).to be(config)
      expect(config.db).to eql("sqlite")
    end

    it "sets new config values in a nested config" do
      klass.setting :db do
        setting :user, default: "root"
        setting :pass, default: "secret"
      end

      klass.config.update(db: {user: "jane", pass: "supersecret"})

      expect(klass.config.db.user).to eql("jane")
      expect(klass.config.db.pass).to eql("supersecret")
    end

    it "runs constructors" do
      klass.setting :db do
        setting :user, default: "root", constructor: ->(v) { v.upcase }
        setting :sslcert, constructor: ->(v) { v&.values_at(:pem, :pass)&.join }
      end

      klass.config.update(db: {user: "jane", sslcert: {pem: "cert", pass: "qwerty"}})

      expect(klass.config.db.user).to eql("JANE")
      expect(klass.config.db.sslcert).to eql("certqwerty")
    end

    it "raises ArgumentError when setting value is not a Hash" do
      klass.setting :db do
        setting :user
      end

      expect { klass.config.update(db: "string") }
        .to raise_error(ArgumentError, '"string" is not a valid setting value')
    end
  end

  describe "#to_h" do
    before do
      klass.setting :db do
        setting :user, default: "root"
        setting :pass, default: "secret"
        setting :ports, default: Set[123, 321]
      end
    end

    it "returns the values as a hash" do
      expect(klass.config.to_h).to eq(
        db: {
          user: "root",
          pass: "secret",
          ports: Set[123, 321]
        }
      )
    end
  end

  describe "#dup" do
    context "with an object" do
      it "returns a deep-copy" do
        klass = Class.new do
          include Dry::Configurable

          setting :db do
            setting :user, default: "root"
            setting :pass, default: "secret"
            setting :ports, default: Set[123]
          end
        end

        parent = Class.new(klass).new
        parent.config.db.ports << 312

        expect(parent.config.db.ports).to eql(Set[123, 312])
        expect(parent.config.dup.db.ports).to eql(Set[123, 312])

        child = Class.new(parent.class).new
        child.config.db.ports << 476

        expect(child.config.db.ports).to eql(Set[123, 476])
        expect(child.config.dup.db.ports).to eql(Set[123, 476])

        expect(klass.new.config.db.ports).to eql(Set[123])
      end

      it "returns an equal copy" do
        klass = Class.new do
          include Dry::Configurable

          setting :db do
            setting :user, default: "root"
            setting :pass, default: "secret"
            setting :ports, default: Set[123]
          end
        end

        object = klass.new

        expect(object.config.dup).to eql(object.config)
      end
    end

    context "with a class" do
      it "returns a deep-copy" do
        klass.setting :db do
          setting :user, default: "root"
          setting :pass, default: "secret"
          setting :ports, default: Set[123]
        end

        parent = Class.new(klass) do
          config.db.ports << 312
        end

        child = Class.new(parent) do
          config.db.ports << 476
        end

        expect(klass.config.db.ports).to eql(Set[123])

        expect(parent.config.db.ports).to eql(Set[123, 312])
        expect(child.config.db.ports).to eql(Set[123, 312, 476])

        expect(parent.config.dup.db.ports).to eql(Set[123, 312])
        expect(child.config.dup.db.ports).to eql(Set[123, 312, 476])
      end

      it "returns an equal copy" do
        klass = Class.new do
          extend Dry::Configurable

          setting :db do
            setting :user, default: "root"
            setting :pass, default: "secret"
            setting :ports, default: Set[123]
          end
        end

        expect(klass.config.dup).to eql(klass.config)
      end
    end
  end

  describe "#[]" do
    it "coerces name from string" do
      klass.setting :db, default: :sqlite

      expect(klass.config["db"]).to eql(:sqlite)
    end

    it "raises ArgumentError when name is not valid" do
      expect { klass.config[:hello] }.to raise_error(ArgumentError, /hello/)
    end
  end

  describe "#hash" do
    it "returns the integer hash value for the convig based on its values" do
      klass.setting :db

      expect(klass.config.hash).to be_an_instance_of(Integer)
      expect { klass.config.db = "sqlite" }.to change { klass.config.hash }
    end

    it "is memoized when the config is finalized", :performance do
      klass.setting :a
      klass.setting :b
      klass.setting :c
      klass.setting :d
      klass.setting :e

      finalized_config = klass.config.dup.finalize!

      expect(finalized_config.hash).to eq klass.config.hash
      expect { finalized_config.hash }.to perform_faster_than { klass.config.hash }.at_least(5).times
    end
  end

  describe "finalize!" do
    it "can be finalized" do
      klass.setting :db do
        setting :user, default: "root".dup
      end

      klass.finalize!
      klass.finalize! # second call becomes a no op

      # It freezes the config and nested config classes
      expect(klass.config).to be_frozen
      expect(klass.config.db).to be_frozen

      # But does not freeze values by default
      expect(klass.config.db.user).not_to be_frozen
      klass.config.db.user << "foo"
      expect(klass.config.db.user).to eq("rootfoo")

      # It does not allow configure block anymore
      expect { klass.configure {} }.to raise_error(Dry::Configurable::FrozenConfigError)
    end

    it "can be finalized with freezing values" do
      klass.setting :db do
        setting :user, default: "root".dup
      end

      klass.finalize!(freeze_values: true)
      klass.finalize!(freeze_values: true) # second call becomes a no op

      expect(klass.config).to be_frozen
      expect(klass.config.db).to be_frozen

      expect(klass.config.db.user).to be_frozen
      expect { klass.config.db.user << "foo" }.to raise_error(FrozenError)

      # does not allow configure block anymore
      expect { klass.configure {} }.to raise_error(Dry::Configurable::FrozenConfigError)
    end

    it "returns early when finalizing a second time" do
      klass.setting :db do
        setting :user, default: "root".dup
      end

      klass.finalize!
      expect(klass.config).to be_frozen
      expect(klass.config.db.user).not_to be_frozen

      # Calling the second time does not do any work
      klass.finalize!(freeze_values: true)
      expect(klass.config.db.user).not_to be_frozen
    end
  end

  describe "#to_data" do
    it "raises if the config is not finalized" do
      klass.setting :db

      expect { klass.config.to_data }.to raise_error(
        Dry::Configurable::FrozenConfigError,
        /must be finalized/
      )
    end

    it "returns a frozen Data instance with a member for every setting" do
      klass.setting :db, default: "sqlite"
      klass.setting :pool, default: 5
      klass.finalize!

      data = klass.config.to_data

      expect(data).to be_frozen
      expect(data).to be_a(Data)
      expect(data.db).to eq("sqlite")
      expect(data.pool).to eq(5)
      expect(data.members).to eq(%i[db pool])
    end

    it "converts nested configs recursively as Data instances" do
      klass.setting :db do
        setting :host, default: "localhost"
        setting :port, default: 5432
      end
      klass.finalize!

      data = klass.config.to_data

      expect(data.db).to be_a(Data)
      expect(data.db).to be_frozen
      expect(data.db.host).to eq("localhost")
      expect(data.db.port).to eq(5432)
    end

    it "returns a fresh but structurally-equal instance per call" do
      klass.setting :db, default: "sqlite"
      klass.finalize!

      first = klass.config.to_data
      second = klass.config.to_data

      expect(first).not_to equal(second)
      expect(first).to eq(second)
      expect(first).to eql(second)
      expect(first.hash).to eq(second.hash)
    end

    it "shares the underlying Data class across configs with the same schema" do
      klass.setting :db
      klass.setting :pool

      first = klass.config.dup.finalize!
      second = klass.config.dup.finalize!

      expect(first.to_data.class).to equal(second.to_data.class)
    end

    it "captures mutable values by reference (not deep-frozen by default)" do
      klass.setting :list, default: []
      klass.finalize!

      klass.config.list << "added after finalize"

      expect(klass.config.to_data.list).to eq(["added after finalize"])
    end
  end

  describe "#to_shareable_data" do
    it "raises if the config is not finalized" do
      klass.setting :db

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::FrozenConfigError,
        /must be finalized/
      )
    end

    it "returns a frozen, Ractor-shareable Data for every supported value type" do
      klass.setting :db, default: "sqlite"
      klass.setting :pool, default: 5
      klass.setting :ratio, default: 1.5
      klass.setting :kind, default: :primary
      klass.setting :enabled, default: true
      klass.setting :missing, default: nil
      klass.setting :name, default: +"hello"
      klass.setting :at, default: Time.at(1_700_000_000)
      klass.setting :list, default: [1, 2]
      klass.setting :opts, default: {host: "localhost"}
      klass.setting :tags, default: Set.new([:a, :b])
      klass.setting :nested do
        setting :deep, default: {list: [1, {x: 2}]}
      end
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data).to be_frozen
      expect(data).to be_a(Data)
      expect(Ractor.shareable?(data)).to be(true)
      expect(data.name).to be_frozen
      expect(data.at).to be_frozen
      expect(data.list).to be_frozen
      expect(data.opts).to be_frozen
      expect(data.opts[:host]).to be_frozen
      expect(data.tags).to be_frozen
      expect(data.nested.deep[:list][1][:x]).to eq(2)
      expect(data.nested.deep).to be_frozen
    end

    it "keeps the snapshot independent of later in-place mutation of the original values" do
      original_list = [1, 2]
      original_hash = {items: original_list}
      klass.setting :list
      klass.setting :options
      klass.config.list = original_list
      klass.config.options = original_hash
      klass.finalize!

      data = klass.config.to_shareable_data

      original_list << 3
      original_hash[:later] = true

      expect(data.list).to eq([1, 2])
      expect(data.options).to eq(items: [1, 2])
    end

    it "is readable from inside a real Ractor" do
      klass.setting :name, default: +"sqlite"
      klass.setting :list, default: [1, 2, 3]
      klass.finalize!

      data = klass.config.to_shareable_data

      result = Ractor.new(data) { |snapshot|
        [snapshot.name, snapshot.list.map { |n| n * 2 }]
      }.take

      expect(result).to eq(["sqlite", [2, 4, 6]])
    end

    it "preserves shared nodes without duplicating them" do
      shared = ["shared"]
      klass.setting :first
      klass.setting :second
      klass.config.first = {node: shared}
      klass.config.second = [shared]
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data.first[:node]).to equal(data.second[0])
    end

    it "preserves cycles within arrays and hashes instead of recursing forever" do
      cyclic_array = []
      cyclic_hash = {back: cyclic_array}
      cyclic_array << cyclic_hash
      klass.setting :cycle
      klass.config.cycle = cyclic_array
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(Ractor.shareable?(data)).to be(true)
      expect(data.cycle[0][:back]).to equal(data.cycle)
    end

    it "preserves cycles that pass through sets" do
      cyclic_hash = {}
      cyclic_set = Set.new([cyclic_hash])
      cyclic_hash[:set] = cyclic_set
      klass.setting :cycle
      klass.config.cycle = cyclic_set
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(Ractor.shareable?(data)).to be(true)
      copied_hash = data.cycle.instance_variable_get(:@hash).keys.first
      expect(copied_hash[:set]).to equal(data.cycle)
    end

    it "preserves a plain (non-proc) hash default" do
      klass.setting :opts
      klass.config.opts = Hash.new(42).tap { |h| h[:set] = 1 }
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data.opts[:missing]).to eq(42)
      expect(Ractor.shareable?(data)).to be(true)
    end

    it "rejects a hash with a default_proc with its config path" do
      klass.setting :db do
        setting :opts, default: Hash.new { |hash, key| hash[key] = key }
      end
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError,
        /config\.db\.opts.*default_proc/
      )
    end

    it "rejects procs with their config path" do
      klass.setting :nested do
        setting :callback, default: -> {}
      end
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError,
        /config\.nested\.callback.*Proc/
      )
    end

    it "rejects IOs with their config path" do
      klass.setting :stream, default: STDOUT
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError,
        /config\.stream.*IO/
      )
    end

    it "rejects unsupported custom objects with their config path" do
      custom_class = Class.new
      klass.setting :thing, default: custom_class.new
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError,
        /config\.thing.*unsupported/
      )
    end

    it "rejects procs nested inside containers with the full path" do
      klass.setting :list, default: [1, -> {}]
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError,
        /config\.list\[1\].*Proc/
      )
    end

    it "does not freeze or mutate any original values when export fails" do
      original_list = [1, 2]
      klass.setting :list
      klass.setting :bad
      klass.config.list = original_list
      klass.config.bad = -> {}
      klass.finalize!

      expect {
        begin
          klass.config.to_shareable_data
        rescue Dry::Configurable::UnsupportedValueError
        end
      }.to_not raise_error

      expect(klass.config.list).not_to be_frozen
      expect(klass.config.list).to eq([1, 2])
      klass.config.list << 3
      expect(klass.config.list).to eq([1, 2, 3])
    end

    it "does not run constructors again for already-resolved values" do
      constructor_calls = 0
      constructor = ->(value) {
        constructor_calls += 1
        value
      }
      klass.setting :list, default: [1, 2], constructor: constructor
      klass.finalize!
      calls_after_finalize = constructor_calls

      klass.config.to_shareable_data

      expect(constructor_calls).to eq(calls_after_finalize)
    end

    it "returns independent snapshots per call" do
      klass.setting :list, default: [1]
      klass.finalize!

      first = klass.config.to_shareable_data
      second = klass.config.to_shareable_data

      expect(first).not_to equal(second)
      expect(first.list).not_to equal(second.list)
      expect(first).to eq(second)
    end

    it "leaves the regular #to_data reference-sharing semantics untouched" do
      original_list = ["x"]
      klass.setting :list
      klass.config.list = original_list
      klass.finalize!

      data = klass.config.to_data
      original_list << "y"

      expect(data.list).to eq(["x", "y"])
      expect(data.list).not_to be_frozen
    end
  end

  describe "#method_missing" do
    it "provides access to reader methods" do
      klass.setting :hello

      expect { klass.config.method(:hello) }.to_not raise_error
    end

    it "provides access to writer methods" do
      klass.setting :hello

      expect { klass.config.method(:hello=) }.to_not raise_error
    end
  end
end
