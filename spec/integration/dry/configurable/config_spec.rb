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

  describe "#snapshot" do
    it "returns a frozen, fully resolved view while the config remains mutable" do
      klass.setting :db, default: "sqlite"
      klass.setting :list, default: []

      view = klass.config.snapshot

      expect(view).to be_frozen
      expect(view.db).to eql("sqlite")
      expect(view.list).to eql([])
      expect(view.values.keys).to contain_exactly(:db, :list)

      expect { view.db = "mysql" }.to raise_error(Dry::Configurable::FrozenConfigError)
      expect { view[:db] = "mysql" }.to raise_error(Dry::Configurable::FrozenConfigError)

      expect(klass.config).not_to be_frozen
      klass.config.db = "mysql"
      expect(klass.config.db).to eql("mysql")
      expect(view.db).to eql("sqlite")
    end

    it "returns a view equal to the config" do
      klass.setting :db do
        setting :ports, default: Set[123]
      end

      expect(klass.config.snapshot).to eql(klass.config)
    end

    it "copies nested configs recursively and freezes them" do
      klass.setting :db do
        setting :ports, default: [123]
      end

      view = klass.config.snapshot

      expect(view.db).to be_frozen

      expect(view.db.ports).to eql([123])

      klass.config.db.ports << 456
      expect(view.db.ports).to eql([123])

      view.db.ports << 789
      expect(klass.config.db.ports).to eql([123, 456])
    end

    it "honors custom copy behavior of values" do
      value_class = Class.new do
        attr_reader :copies

        def initialize
          @copies = 0
        end

        def initialize_copy(source)
          super
          @copies = source.copies + 1
        end
      end

      value = value_class.new
      klass.setting :custom, default: value

      view = klass.config.snapshot

      expect(view.custom).not_to equal(value)
      expect(view.custom.copies).to eq(1)
      expect(value.copies).to eq(0)
    end

    it "preserves shared references within each view while keeping views independent" do
      shared = {mode: "a"}
      klass.setting :first, default: [shared]
      klass.setting :second, default: [shared]

      # The config's own values share the sub-value through one array layer
      expect(klass.config.first.first).to equal(klass.config.second.first)

      first_view = klass.config.snapshot

      # In-place mutation on the original config after the first view was taken
      klass.config.first.first[:mode] = "b"

      second_view = klass.config.snapshot

      # Each view sees the same sharing relationship internally
      expect(first_view.first.first).to equal(first_view.second.first)
      expect(second_view.first.first).to equal(second_view.second.first)

      # The first view retains the values in effect when it was taken
      expect(first_view.first.first[:mode]).to eql("a")
      expect(second_view.first.first[:mode]).to eql("b")

      # Writes through a view do not propagate back to the config
      first_view.first.first[:mode] = "z"
      first_view.first << :extra
      expect(klass.config.first.first[:mode]).to eql("b")
      expect(klass.config.first).to eql([{mode: "b"}])

      # Taking views did not freeze the source values
      expect(klass.config.first.first).not_to be_frozen
      klass.config.first.first[:mode] = "c"
      expect(klass.config.second.first[:mode]).to eql("c")
    end

    it "does not freeze the config or its values" do
      klass.setting :list, default: []

      klass.config.snapshot

      expect(klass.config).not_to be_frozen
      expect(klass.config.list).not_to be_frozen
      klass.config.list << "still mutable"
      expect(klass.config.list).to eql(["still mutable"])
    end

    it "captures frozen values as-is" do
      klass.setting :env, default: "prod".freeze

      view = klass.config.snapshot

      expect(view.env).to equal(klass.config.env)
    end

    it "still allows the config to be finalized afterwards" do
      klass.setting :db, default: "sqlite"

      klass.config.snapshot
      klass.finalize!

      expect(klass.config).to be_frozen
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
