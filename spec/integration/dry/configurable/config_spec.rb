# frozen_string_literal: true

require "pathname"

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

    it "returns a frozen, Ractor-shareable Data instance" do
      klass.setting :db, default: "sqlite"
      klass.setting :pool, default: 5
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data).to be_a(Data)
      expect(data).to be_frozen
      expect(data.db).to eq("sqlite")
      expect(data.pool).to eq(5)
      expect(Ractor.shareable?(data)).to be(true)
    end

    it "converts nested configs recursively to Data instances" do
      klass.setting :db do
        setting :host, default: "localhost"
        setting :port, default: 5432
      end
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data.db).to be_a(Data)
      expect(data.db).to be_frozen
      expect(Ractor.shareable?(data)).to be(true)
      expect(data.db.host).to eq("localhost")
      expect(data.db.port).to eq(5432)
    end

    it "copies mutable values, so later mutation of the config does not change the snapshot" do
      klass.setting :list, default: %w[a b]
      klass.setting :deep, default: {tags: [1]}
      klass.setting :word, default: String.new("hello")
      klass.setting :at, default: Time.utc(2020, 1, 1)
      klass.setting :items, default: Set[1, 2]
      klass.finalize!

      original_time = klass.config.at
      data = klass.config.to_shareable_data

      klass.config.list << "c"
      klass.config.deep[:tags] << 2
      klass.config.word << "!"
      klass.config.items << 3

      expect(data.list).to eq(%w[a b])
      expect(data.deep).to eq(tags: [1])
      expect(data.word).to eq("hello")
      expect(data.items).to eq(Set[1, 2])
      expect(data.at).to eq(Time.utc(2020, 1, 1))
      expect(data.at).to_not equal(original_time)

      [data.list, data.deep, data.deep[:tags], data.word, data.at, data.items].each do |value|
        expect(value).to be_frozen
      end

      expect(klass.config.list).to eq(%w[a b c])
      expect(klass.config.word).to eq("hello!")
      expect(klass.config).to be_frozen
      expect(klass.config.list).to_not be_frozen
    end

    it "is readable from inside a real Ractor" do
      klass.setting :db do
        setting :host, default: "localhost"
      end
      klass.setting :list, default: %w[a b]
      klass.finalize!

      data = klass.config.to_shareable_data

      result = Ractor.new(data) do |snapshot|
        [snapshot.db.host, snapshot.list]
      end.take

      expect(result).to eq(["localhost", %w[a b]])
    end

    it "preserves repeated references as a single shared node" do
      shared = {tags: [1, 2]}
      klass.setting :a, default: nil
      klass.setting :b, default: nil
      klass.config.a = shared
      klass.config.b = shared
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data.a).to equal(data.b)
      expect(data.a[:tags]).to be_frozen

      shared[:tags] << 3
      expect(data.a[:tags]).to eq([1, 2])
    end

    it "preserves cycles nested inside containers" do
      graph = {name: "root"}
      graph[:self] = graph
      nodes = Set[graph]
      graph[:nodes] = nodes

      klass.setting :graph, default: nil
      klass.config.graph = graph
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data.graph[:self]).to equal(data.graph)
      expect(data.graph[:nodes]).to include(data.graph)
      expect(data.graph).to be_frozen
      expect(Ractor.shareable?(data)).to be(true)
    end

    it "converts a nested config stored inside a container without duplicating it" do
      klass.setting :db do
        setting :host, default: "localhost"
      end
      klass.setting :places, default: nil
      nested = klass.config.db
      klass.config.places = [nested, nested]
      klass.finalize!

      data = klass.config.to_shareable_data

      expect(data.places[0]).to be_a(Data)
      expect(data.places[0]).to equal(data.places[1])
      expect(data.places[0]).to equal(data.db)
      expect(Ractor.shareable?(data)).to be(true)
    end

    it "rejects a Hash with a default_proc with the locatable config path" do
      klass.setting :nest do
        setting :items, default: [Hash.new { |h, k| h[k] = [] }]
      end
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError,
        /at nest\.items\[0\]: .*default_proc/
      )
    end

    it "rejects IO values with the locatable config path" do
      klass.setting :io, default: $stdout
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError, /at io: .*IO/
      )
    end

    it "rejects Proc values with the locatable config path" do
      klass.setting :block, default: proc { 1 }
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError, /at block: .*Proc/
      )
    end

    it "rejects unsupported custom objects with the locatable config path" do
      klass.setting :nest do
        setting :deep, default: Object.new
      end
      klass.finalize!

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError, /at nest\.deep: .*Object/
      )
    end

    it "does not freeze or mutate the original config or its values when the export fails" do
      bad = Object.new

      klass.setting :bad, default: nil
      klass.setting :list, default: ["a"]
      klass.config.bad = bad
      klass.finalize!

      list = klass.config.list

      expect { klass.config.to_shareable_data }.to raise_error(
        Dry::Configurable::UnsupportedValueError
      )

      expect(bad).to_not be_frozen
      expect(list).to_not be_frozen
      expect(list).to eq(["a"])

      list << "b"
      expect(klass.config.list).to eq(%w[a b])
    end

    it "does not run a setting's constructor again for already-constructed values" do
      constructor_calls = 0
      constructor = -> value {
        constructor_calls += 1
        value
      }

      klass.setting :pool, default: 5, constructor: constructor
      klass.finalize!

      expect {
        klass.config.to_shareable_data
        klass.config.to_shareable_data
      }.to_not change { constructor_calls }
    end

    it "leaves #to_data capturing mutable values by reference" do
      klass.setting :list, default: []
      klass.finalize!

      klass.config.list << "after finalize"

      expect(klass.config.to_data.list).to eq(["after finalize"])
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
