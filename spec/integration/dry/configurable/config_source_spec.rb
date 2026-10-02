# frozen_string_literal: true

RSpec.describe Dry::Configurable::Config, "#source" do
  subject(:config) do
    klass.config
  end

  let(:klass) do
    Class.new do
      extend Dry::Configurable

      setting :timeout, default: 30
      setting :tags, default: []
      setting :db do
        setting :host, default: "localhost"
        setting :port, default: 5432
      end
    end
  end

  def source(...)
    config.source(...)
  end

  describe "unconfigured settings" do
    it "reports a non-explicit default origin" do
      expect(source(:timeout)).to have_attributes(
        origin: :default,
        explicit?: false,
        default?: true,
        inherited?: false,
        local?: false
      )
    end

    it "stays default after the value is read" do
      config.timeout

      expect(source(:timeout)).to have_attributes(origin: :default, explicit?: false)
    end

    it "does not record file names or call stacks" do
      result = source(:timeout)

      expect(result).to respond_to(:origin)
      expect(result).to respond_to(:explicit)
      expect(result.instance_variables).to contain_exactly(:@origin, :@explicit)
    end
  end

  describe "explicit assignment" do
    it "reports a local, explicit source" do
      config.timeout = 15

      expect(source(:timeout)).to have_attributes(origin: :local, explicit?: true)
    end

    it "stays explicit when the value happens to equal the default" do
      config.timeout = 30

      expect(source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(config.configured?(:timeout)).to be true
    end

    it "marks values assigned through #update as local and explicit" do
      config.update(timeout: 20)

      expect(source(:timeout)).to have_attributes(origin: :local, explicit?: true)
    end

    it "marks values assigned through #configure as local and explicit" do
      klass.configure { |c| c.timeout = 25 }

      expect(source(:timeout)).to have_attributes(origin: :local, explicit?: true)
    end
  end

  describe "mutable defaults" do
    it "does not count reading the array as an assignment" do
      config.tags

      expect(source(:tags)).to have_attributes(origin: :default, explicit?: false)
      expect(config.configured?(:tags)).to be false
    end

    it "distinguishes an untouched default from an in-place mutation" do
      config.tags << :a

      # The mutation is observable through `configured?`, but there was still no explicit
      # assignment, so the source remains default/non-explicit.
      expect(config.configured?(:tags)).to be true
      expect(source(:tags)).to have_attributes(origin: :default, explicit?: false)
    end

    it "becomes local and explicit once explicitly assigned" do
      config.tags = [:a]

      expect(source(:tags)).to have_attributes(origin: :local, explicit?: true)
    end

    it "stays explicit when assigned an array equal to the default" do
      config.tags = []

      expect(source(:tags)).to have_attributes(origin: :local, explicit?: true)
      expect(config.configured?(:tags)).to be false
    end
  end

  describe "nested settings" do
    it "resolves a path given as multiple keys" do
      expect(config.source(:db, :host)).to have_attributes(origin: :default, explicit?: false)
    end

    it "resolves a dot-separated path" do
      expect(config.source("db.host")).to have_attributes(origin: :default, explicit?: false)
    end

    it "reports nested leaves independently" do
      config.db.port = 6000

      expect(config.source(:db, :port)).to have_attributes(origin: :local, explicit?: true)
      expect(config.source(:db, :host)).to have_attributes(origin: :default, explicit?: false)
    end

    it "does not mark a nested leaf configured merely by querying its source" do
      config.source(:db, :host)

      expect(config.configured?(:db)).to be false
      expect(config.db.configured?(:host)).to be false
    end

    it "raises when an intermediate setting is not nested" do
      expect { config.source(:timeout, :nope) }
        .to raise_error(ArgumentError, /\+timeout\+ is not a nested setting/)
    end
  end

  describe "inheritance" do
    let(:subclass) { Class.new(klass) }

    before do
      config.timeout = 15
      config.tags << :a
      config.db.port = 6000
      subclass
    end

    it "marks inherited, explicitly assigned values as inherited and explicit" do
      expect(subclass.config.source(:timeout)).to have_attributes(
        origin: :inherited, explicit?: true
      )
    end

    it "marks inherited mutations as inherited but non-explicit" do
      expect(subclass.config.source(:tags)).to have_attributes(
        origin: :inherited, explicit?: false
      )
    end

    it "inherits nested configured leaves while untouched leaves stay default" do
      expect(subclass.config.source(:db, :port)).to have_attributes(
        origin: :inherited, explicit?: true
      )
      expect(subclass.config.source(:db, :host)).to have_attributes(
        origin: :default, explicit?: false
      )
    end

    it "does not carry read-only materialized values into the subclass" do
      other = Class.new(Class.new { extend Dry::Configurable; setting :only_read, default: 1 })
      other.superclass.config.only_read

      expect(other.config.source(:only_read)).to have_attributes(
        origin: :default, explicit?: false
      )
      expect(other.config.configured?(:only_read)).to be false
    end

    it "switches to local once the subclass assigns the value itself" do
      subclass.config.timeout = 99

      expect(subclass.config.source(:timeout)).to have_attributes(
        origin: :local, explicit?: true
      )
    end

    it "leaves the parent source untouched when a subclass assigns locally" do
      subclass.config.timeout = 99

      expect(config.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
    end

    it "leaves a sibling's inherited source untouched when one subclass assigns locally" do
      first = Class.new(klass)
      second = Class.new(klass)

      first.config.timeout = 7

      expect(second.config.source(:timeout)).to have_attributes(
        origin: :inherited, explicit?: true
      )
    end

    it "does not retroactively change a subclass when the parent is later changed" do
      late_parent = Class.new { extend Dry::Configurable; setting :timeout, default: 30 }
      child = Class.new(late_parent)

      late_parent.config.timeout = 1

      expect(child.config.source(:timeout)).to have_attributes(
        origin: :default, explicit?: false
      )
    end
  end

  describe "#dup" do
    before do
      config.timeout = 15
      config.tags << :a
      config.db.port = 6000
    end

    it "preserves sources for top-level and nested settings" do
      copy = config.dup

      expect(copy.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(copy.source(:tags)).to have_attributes(origin: :default, explicit?: false)
      expect(copy.source(:db, :port)).to have_attributes(origin: :local, explicit?: true)
      expect(copy.source(:db, :host)).to have_attributes(origin: :default, explicit?: false)
    end

    it "does not change the original's source when the copy is assigned" do
      copy = config.dup
      copy.timeout = 99

      expect(copy.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(config.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(copy.timeout).to eq(99)
      expect(config.timeout).to eq(15)
    end
  end

  describe "#pristine" do
    before do
      config.timeout = 15
      config.tags << :a
      config.db.port = 6000
    end

    it "resets every source back to the default" do
      pristine = config.pristine

      expect(pristine.source(:timeout)).to have_attributes(origin: :default, explicit?: false)
      expect(pristine.source(:tags)).to have_attributes(origin: :default, explicit?: false)
      expect(pristine.source(:db, :port)).to have_attributes(origin: :default, explicit?: false)
      expect(pristine.source(:db, :host)).to have_attributes(origin: :default, explicit?: false)
    end

    it "does not change the original config's sources" do
      config.pristine

      expect(config.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(config.source(:db, :port)).to have_attributes(origin: :local, explicit?: true)
    end
  end

  describe "#finalize! and #to_data" do
    before do
      config.timeout = 15
      config.tags << :a
      config.db.port = 6000
    end

    it "preserves sources through finalization" do
      config.finalize!

      expect(config.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(config.source(:tags)).to have_attributes(origin: :default, explicit?: false)
      expect(config.source(:db, :port)).to have_attributes(origin: :local, explicit?: true)
      expect(config.source(:db, :host)).to have_attributes(origin: :default, explicit?: false)
    end

    it "preserves sources through a finalized #to_data round trip source lookup" do
      finalized = config.dup.finalize!
      data = finalized.to_data

      expect(data.timeout).to eq(15)
      expect(data.db.port).to eq(6000)
      expect(finalized.source(:timeout)).to have_attributes(origin: :local, explicit?: true)
      expect(finalized.source(:db, :port)).to have_attributes(origin: :local, explicit?: true)
    end

    it "reports the source of a value captured by reference after finalization" do
      finalized = config.dup.finalize!
      finalized.tags << :b

      expect(finalized.to_data.tags).to eq(%i[a b])
      expect(finalized.source(:tags)).to have_attributes(origin: :default, explicit?: false)
    end
  end

  describe "invalid paths" do
    it "raises for an unknown top-level setting" do
      expect { config.source(:nope) }
        .to raise_error(ArgumentError, /\+nope\+ is not a setting name/)
    end

    it "raises for an unknown nested setting" do
      expect { config.source(:db, :nope) }
        .to raise_error(ArgumentError, /\+nope\+ is not a setting name/)
    end

    it "raises when no path is given" do
      expect { config.source }
        .to raise_error(ArgumentError, /a setting path is required/)
    end
  end
end
