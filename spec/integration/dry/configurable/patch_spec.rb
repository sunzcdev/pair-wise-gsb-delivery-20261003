# frozen_string_literal: true

RSpec.describe Dry::Configurable::Patch do
  let(:klass) do
    Class.new do
      extend Dry::Configurable

      setting :flat, default: "flat-default"

      setting :db do
        setting :host, default: "localhost"
        setting :port, default: 5432
        setting :user, default: "root"
        setting :note, default: nil

        setting :cred do
          setting :user, default: nil
          setting :pass, default: nil
        end
      end

      setting :strict, default: 1, constructor: ->(value) {
        raise ArgumentError, "not an integer: #{value.inspect}" unless value.is_a?(Integer)

        value
      }
    end
  end

  let(:config) { klass.config.dup }

  describe "explicit writes" do
    it "pins a top-level setting to a value" do
      config.apply_patch(flat: "pinned")

      expect(config.flat).to eql("pinned")
      expect(config).to be_configured(:flat)
    end

    it "pins a nested setting through a dot path" do
      config.apply_patch("db.host" => "example.com")

      expect(config.db.host).to eql("example.com")
      expect(config.db).to be_configured(:host)
    end

    it "reaches deeply nested settings" do
      config.apply_patch("db.cred.pass" => "secret")

      expect(config.db.cred.pass).to eql("secret")
      expect(config.db.cred).to be_configured(:pass)
    end

    it "treats explicit nil as a value, not an action" do
      config.apply_patch("db.note" => nil)

      expect(config.db.note).to be_nil
      expect(config.db).to be_configured(:note)
    end

    it "runs the setting constructor once and stores its result" do
      calls = 0
      klass.setting :doubled, default: 0, constructor: ->(value) { calls += 1; value * 2 }

      config.apply_patch(doubled: 21)

      expect(config.doubled).to eql(42)
      expect(calls).to be(1)
    end
  end

  describe "Dry::Configurable::INHERIT" do
    it "releases an override so the setting follows its default" do
      config.db.host = "written"

      config.apply_patch("db.host" => Dry::Configurable::INHERIT)

      expect(config.db.host).to eql("localhost")
      expect(config.db).not_to be_configured(:host)
    end

    it "is a no-op on a setting without an override" do
      config.apply_patch("db.host" => Dry::Configurable::INHERIT)

      expect(config.db.host).to eql("localhost")
      expect(config.db).not_to be_configured(:host)
    end

    it "re-follows the default after the common default changes" do
      config.db.host # capture the current default

      klass.settings[:db].children[:host].instance_variable_set(:@default, "newhost")
      expect(config.db.host).to eql("localhost") # captured value stays put until released

      config.apply_patch("db.host" => Dry::Configurable::INHERIT)

      expect(config.db.host).to eql("newhost")
    end

    it "is rejected when the setting has no defined default" do
      klass_without_default = Class.new do
        extend Dry::Configurable(default_undefined: true)

        setting :missing
      end

      expect {
        klass_without_default.config.dup.apply_patch(missing: Dry::Configurable::INHERIT)
      }.to raise_error(Dry::Configurable::InvalidPatchValueError, /no defined default to inherit/)
    end
  end

  describe "Dry::Configurable::REMOVE_OVERRIDE" do
    it "drops an explicit override and its cached value" do
      config.db.port = 9999
      expect(config.db).to be_configured(:port)

      config.apply_patch("db.port" => Dry::Configurable::REMOVE_OVERRIDE)

      expect(config.db.port).to be(5432)
      expect(config.db).not_to be_configured(:port)
    end

    it "works even when the default is Undefined" do
      klass_undefined = Class.new do
        extend Dry::Configurable(default_undefined: true)

        setting :optional
      end

      cfg = klass_undefined.config.dup
      cfg.optional = "value"

      cfg.apply_patch(optional: Dry::Configurable::REMOVE_OVERRIDE)

      expect(cfg.optional).to be(Dry::Configurable::Undefined)
      expect(cfg).not_to be_configured(:optional)
    end

    it "re-follows the default after the common default changes" do
      config.db.port = 9999
      klass.settings[:db].children[:port].instance_variable_set(:@default, 6543)

      config.apply_patch("db.port" => Dry::Configurable::REMOVE_OVERRIDE)

      expect(config.db.port).to be(6543)
    end
  end

  describe "atomicity" do
    it "applies no entries when a later path is unknown" do
      snapshot = config.to_h

      expect {
        config.apply_patch("flat" => "pinned", "db.unknown" => 1)
      }.to raise_error(Dry::Configurable::InvalidPatchPathError, /db\.unknown/)

      expect(config.to_h).to eql(snapshot)
      expect(config).not_to be_configured(:flat)
    end

    it "applies no entries when an earlier path is unknown" do
      expect {
        config.apply_patch("nope" => 1, "flat" => "pinned")
      }.to raise_error(Dry::Configurable::InvalidPatchPathError, /nope/)

      expect(config.flat).to eql("flat-default")
      expect(config).not_to be_configured(:flat)
    end

    it "rejects a path that traverses through a non-nested setting" do
      expect {
        config.apply_patch("flat.deeper" => 1)
      }.to raise_error(Dry::Configurable::InvalidPatchPathError, /not a nested setting/)
    end

    it "rejects pinning a nested setting to a scalar value" do
      expect {
        config.apply_patch(db: "not-a-hash")
      }.to raise_error(Dry::Configurable::InvalidPatchValueError, /nested setting/)
    end

    it "applies no entries when a later value is rejected by a constructor" do
      snapshot = config.to_h

      expect {
        config.apply_patch(flat: "pinned", strict: "nope")
      }.to raise_error(Dry::Configurable::InvalidPatchValueError, /rejected by its constructor/)

      expect(config.to_h).to eql(snapshot)
      expect(config).not_to be_configured(:flat)
      expect(config).not_to be_configured(:strict)
    end

    it "applies no entries when an earlier value is rejected by a constructor" do
      expect {
        config.apply_patch(strict: "nope", flat: "pinned")
      }.to raise_error(Dry::Configurable::InvalidPatchValueError)

      expect(config.flat).to eql("flat-default")
      expect(config.strict).to be(1)
    end

    it "leaves no read-capture trace behind when validation fails on a nested path" do
      expect {
        config.apply_patch("db.cred.pass" => "x", "db.cred.missing" => "y")
      }.to raise_error(Dry::Configurable::InvalidPatchPathError)

      expect(config.db).not_to be_configured(:cred)
      expect(config.db.cred).not_to be_configured(:pass)
    end

    it "does not modify a finalized config" do
      config.finalize!

      expect {
        config.apply_patch(flat: "pinned")
      }.to raise_error(Dry::Configurable::FrozenConfigError)
    end
  end

  describe "the Patch object" do
    it "can be built with the DSL and reused" do
      patch = described_class.new
        .set("db.host", "example.com")
        .inherit("db.user")
        .remove_override("db.port")

      other = klass.config.dup
      other.db.host = "old"
      other.db.user = "someone"
      other.db.port = 1

      config.apply_patch(patch)
      other.apply_patch(patch)

      expect(config.db.host).to eql("example.com")
      expect(other.db.host).to eql("example.com")
      expect(config.db.user).to eql("root")
      expect(other.db.user).to eql("root")
      expect(config.db.port).to be(5432)
      expect(other.db.port).to be(5432)
    end

    it "supports the [] shorthand" do
      patch = described_class["db.host" => "example.com"]

      expect { config.apply_patch(patch) }.to change { config.db.host }.to("example.com")
    end

    it "compares by entries" do
      expect(described_class.new("db.host" => "x"))
        .to eql(described_class.new("db.host" => "x"))
    end

    it "preserves insertion order in its entries" do
      patch = described_class.new(flat: "a", "db.host" => "b")

      expect(patch.entries.map(&:first)).to eql(%w[flat db.host])
    end
  end

  describe "synchronization" do
    it "keeps equal effective outputs while provenance differs, then diverges on default changes" do
      first = klass.config.dup
      second = klass.config.dup

      # The first config previously overrode one nested child, with the same
      # effective value as its default.
      first.db.user = "root"

      # Sync the FIRST config only: explicitly pin one nested child to its
      # current value, release the override on another, and explicitly keep one
      # value as nil.
      first.apply_patch(
        "db.host" => "localhost",
        "db.user" => Dry::Configurable::REMOVE_OVERRIDE,
        "db.note" => nil
      )

      # Effective outputs of the touched settings are identical to the pristine
      # second config. Provenance is checked below via #configured? without
      # resolving (and thus capturing) the values before the defaults change.
      expect(first.db.configured?(:host)).to be(true)
      expect(second.db.configured?(:host)).to be(false)
      expect(first.db.configured?(:user)).to be(false)
      expect(second.db.configured?(:user)).to be(false)
      expect(first.db.configured?(:note)).to be(true)
      expect(second.db.configured?(:note)).to be(false)

      # The common defaults change.
      klass.settings[:db].children[:host].instance_variable_set(:@default, "newhost")
      klass.settings[:db].children[:user].instance_variable_set(:@default, "admin")
      klass.settings[:db].children[:port].instance_variable_set(:@default, 6543)

      # Items that should follow the default follow it in BOTH configs: the
      # released override in the first and the pristine value in the second, as
      # well as a setting neither config ever touched.
      expect(first.db.user).to eql("admin")
      expect(second.db.user).to eql("admin")
      expect(first.db.port).to eql(6543)
      expect(second.db.port).to eql(6543)

      # The explicitly pinned item stays fixed in the first and follows in the second.
      expect(first.db.host).to eql("localhost")
      expect(second.db.host).to eql("newhost")

      # The explicit nil remains an explicit, configured value in the first.
      expect(first.db.note).to be_nil
      expect(first.db).to be_configured(:note)
    end

    it "keeps original values and override ownership when the last entry is rejected" do
      first = klass.config.dup
      first.apply_patch(
        "db.host" => "pinned-host",
        "db.note" => nil
      )

      snapshot = first.to_h
      ownership = {
        host: first.db.configured?(:host),
        port: first.db.configured?(:port),
        note: first.db.configured?(:note)
      }

      expect {
        first.apply_patch(
          "db.user" => "writer",       # writable first entry
          strict: "not-an-integer"     # rejected last entry
        )
      }.to raise_error(Dry::Configurable::InvalidPatchValueError)

      expect(first.to_h).to eql(snapshot)
      expect({
        host: first.db.configured?(:host),
        port: first.db.configured?(:port),
        note: first.db.configured?(:note)
      }).to eql(ownership)
      expect(first.db).not_to be_configured(:user)
    end
  end
end
