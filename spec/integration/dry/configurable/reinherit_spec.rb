# frozen_string_literal: true

RSpec.describe Dry::Configurable, "re-inheriting config values from ancestors" do
  let(:parent) do
    Class.new do
      extend Dry::Configurable

      setting :read_list, default: [:read]
      setting :explicit_list, default: [:explicit]
      setting :nullable, default: :fallback
      setting :nested do
        setting :list, default: %i[one two]
        setting :other, default: :original
      end
    end
  end

  let(:middle) { Class.new(parent) }
  let(:grandchild) { Class.new(middle) }
  let(:sibling) { Class.new(middle) }

  before do
    [parent, middle, grandchild, sibling].each do |klass|
      klass.config.read_list                   # read only
      klass.config.explicit_list = [:explicit] # explicit assignment equal to the old default
      klass.config.nullable = nil              # explicit nil
      klass.config.nested.list[0] = :mutated   # in-place change of a nested array element
    end
  end

  def update_parent_defaults
    parent.config.read_list = [:read_v2]
    parent.config.nested.other = :other_v2
  end

  describe "#reinherit_config!" do
    before do
      # Parent defaults move twice, then one grandchild re-inherits
      update_parent_defaults
      update_parent_defaults
      parent.config.read_list = [:read_v3]
      parent.config.nested.other = :other_v3

      grandchild.reinherit_config!
    end

    it "lets read-only values follow to the latest ancestor values" do
      expect(grandchild.config.read_list).to eql([:read_v3])
      expect(grandchild.config.nested.other).to be(:other_v3)
    end

    it "keeps explicit assignments of the old default as overrides" do
      expect(grandchild.config.explicit_list).to eql([:explicit])
    end

    it "keeps explicit nils as overrides" do
      expect(grandchild.config.nullable).to be_nil
    end

    it "keeps in-place changes of nested values as overrides" do
      expect(grandchild.config.nested.list).to eql(%i[mutated two])
    end

    it "does not modify the ancestors or siblings" do
      expect(parent.config.read_list).to eql([:read_v3])
      expect(parent.config.nested.other).to be(:other_v3)

      # The middle class and the sibling never re-inherited, so their read-only
      # values still reflect the defaults captured at subclassing time
      expect(middle.config.read_list).to eql([:read])
      expect(middle.config.nested.other).to be(:original)
      expect(sibling.config.read_list).to eql([:read])
      expect(sibling.config.nested.other).to be(:original)
    end

    it "gives the re-inheriting branch its own mutable data" do
      grandchild.config.read_list << :local
      grandchild.config.nested.list << :three

      expect(grandchild.config.read_list).to eql([:read_v3, :local])
      expect(grandchild.config.nested.list).to eql(%i[mutated two three])

      expect(parent.config.read_list).to eql([:read_v3])
      expect(parent.config.nested.list).to eql(%i[mutated two])
      expect(middle.config.read_list).to eql([:read])
      expect(sibling.config.read_list).to eql([:read])
      expect(sibling.config.nested.list).to eql(%i[mutated two])
    end

    it "keeps following when ancestors update again and the config re-inherits again" do
      expect(grandchild.config.read_list).to eql([:read_v3])

      parent.config.read_list = [:read_v4]
      grandchild.reinherit_config!

      expect(grandchild.config.read_list).to eql([:read_v4])
    end

    it "stops following a value once it is mutated in place after re-inheriting" do
      grandchild.config.read_list << :local

      parent.config.read_list = [:read_v4]
      grandchild.reinherit_config!

      expect(grandchild.config.read_list).to eql([:read_v3, :local])
    end

    it "stops following a value once it is explicitly assigned after re-inheriting" do
      grandchild.config.read_list = [:mine]

      parent.config.read_list = [:read_v4]
      grandchild.reinherit_config!

      expect(grandchild.config.read_list).to eql([:mine])
    end

    it "returns self" do
      expect(middle.reinherit_config!).to be(middle)
    end

    it "preserves normal inheritance: new subclasses copy the re-inherited config" do
      subclass = Class.new(grandchild)

      expect(subclass.config.read_list).to eql([:read_v3])
      expect(subclass.config.explicit_list).to eql([:explicit])
      expect(subclass.config.nullable).to be_nil
      expect(subclass.config.nested.list).to eql(%i[mutated two])

      subclass.config.read_list << :sub
      expect(grandchild.config.read_list).to eql([:read_v3])
    end

    it "preserves the finalize! conventions" do
      grandchild.finalize!

      expect(grandchild.config).to be_frozen
      expect { grandchild.reinherit_config! }.to raise_error(
        Dry::Configurable::FrozenConfigError,
        "Cannot modify frozen config"
      )
    end
  end

  describe "#reinherit_config! with no ancestor updates" do
    it "resets read-only values to a fresh default" do
      grandchild.reinherit_config!

      expect(grandchild.config.read_list).to eql([:read])
      expect(grandchild.config.nested.other).to be(:original)
      expect(grandchild.config.explicit_list).to eql([:explicit])
      expect(grandchild.config.nullable).to be_nil
      expect(grandchild.config.nested.list).to eql(%i[mutated two])
    end
  end

  describe "Config#reinherit!" do
    it "follows a single given config" do
      parent.config.read_list = [:read_v2]

      grandchild.config.reinherit!(parent.config)

      expect(grandchild.config.read_list).to eql([:read_v2])
      expect(grandchild.config.explicit_list).to eql([:explicit])
    end

    it "prefers the nearest source overriding a setting" do
      middle.config.read_list = [:middle]
      parent.config.read_list = [:read_v2]

      grandchild.config.reinherit!([middle.config, parent.config])

      expect(grandchild.config.read_list).to eql([:middle])
    end
  end

  describe "classes including Dry::Configurable" do
    it "is a no-op without class-level config" do
      klass = Class.new do
        include Dry::Configurable

        setting :read_list, default: [:read]
      end

      expect(klass.reinherit_config!).to be(klass)
      expect(klass.new.config.read_list).to eql([:read])
    end
  end
end
