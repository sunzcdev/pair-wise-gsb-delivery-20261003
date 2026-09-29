# frozen_string_literal: true

require "dry/validation/contract"

RSpec.describe Dry::Validation::Contract, "rule purposes" do
  subject(:contract) { contract_class.new }

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      def self.name
        "PurposeContract"
      end

      params do
        required(:name).filled(:string)
        optional(:consent).maybe(:bool)
        optional(:details).maybe(:string)
        optional(:summary).maybe(:string)
      end

      rule(:name) do
        key.failure("name too short") if value.length < 2
      end

      rule(:consent).for(:submit) do
        key.failure("must accept consent") unless value == true
      end

      rule(:details).for(:submit) do
        key.failure("details are required on submit") if values[:details].to_s.empty?
      end

      rule(:summary).for(:draft).depends(:details) do
        key.failure("summary must follow details") unless value.to_s.start_with?(values[:details].to_s)
      end
    end
  end

  describe "selecting a single purpose" do
    it "runs base rules and rules tagged with the requested purpose" do
      result = contract.(name: "ab", purposes: :submit)

      expect(result.rule_selection.applied_keys).to eql(%i[name consent details])
      expect(result.rule_selection.omitted_keys).to eql(%i[summary])
    end

    it "omits rules that belong to other purposes" do
      result = contract.(name: "ab", purposes: :draft)

      expect(result.rule_selection.applied_keys).to eql(%i[name details summary])
      expect(result.rule_selection.omitted_keys).to eql(%i[consent])
    end

    it "does not apply failures from omitted rules" do
      result = contract.(name: "ab", details: "de", summary: "de", purposes: :draft)

      expect(result.errors.to_h).to eql({})
    end

    it "applies failures from selected rules" do
      result = contract.(name: "ab", purposes: :submit)

      expect(result.errors.to_h).to eql(consent: ["must accept consent"], details: ["details are required on submit"])
    end
  end

  describe "running without purposes" do
    it "only runs base rules" do
      result = contract.(name: "ab")

      expect(result.rule_selection.applied_keys).to eql(%i[name])
      expect(result.rule_selection.omitted_keys).to eql(%i[consent details summary])
    end
  end

  describe "selecting several purposes" do
    it "unions the selected rules" do
      result = contract.(name: "ab", details: "de", summary: "de", purposes: %i[draft submit])

      expect(result.rule_selection.applied_keys).to eql(%i[name consent details summary])
      expect(result.rule_selection.omitted_keys).to eql([])
    end
  end

  describe "explicit cross-purpose dependencies" do
    it "pulls a dependency in even when its purpose was not requested" do
      result = contract.(name: "ab", summary: "x", details: "de", purposes: :draft)

      entry = result.rule_selection.entry_for(:details)

      expect(entry.reason).to be(:dependency)
      expect(entry.source).to be(:summary)
    end

    it "evaluates dependencies before the rules that need them" do
      result = contract.(name: "ab", summary: "x", details: "de", purposes: :draft)

      expect(result.rule_selection.applied_keys.index(:details))
        .to be < result.rule_selection.applied_keys.index(:summary)
    end

    it "raises when a dependency points at an unknown rule" do
      klass = Class.new(Dry::Validation::Contract) do
        params { required(:a).filled(:string) }

        rule(:a).for(:submit).depends(:nope) {}
      end

      expect { klass.new.({a: "x"}, purposes: :submit) }
        .to raise_error(Dry::Validation::InvalidDependencyError, /unknown rule/)
    end

    it "raises on a dependency cycle" do
      klass = Class.new(Dry::Validation::Contract) do
        params do
          required(:a).filled(:string)
          required(:b).filled(:string)
        end

        rule(:a).for(:submit).depends(:b) {}
        rule(:b).for(:submit).depends(:a) {}
      end

      expect { klass.new.({a: "x", b: "y"}, purposes: :submit) }
        .to raise_error(Dry::Validation::CircularDependencyError, /circular/)
    end
  end

  describe "mandatory rules" do
    let(:contract_class) do
      Class.new(Dry::Validation::Contract) do
        params { required(:name).filled(:string) }

        rule(:name).for(:draft).required_when(:submit) do
          key.failure("name must be exactly ok on submit") unless value == "ok"
        end
      end
    end

    it "forces the rule for a purpose it is not tagged with" do
      result = contract.(name: "nope", purposes: :submit)

      expect(result.rule_selection.applied_keys).to eql(%i[name])
      expect(result.rule_selection.entry_for(:name).reason).to be(:mandatory)
      expect(result.errors.to_h).to eql(name: ["name must be exactly ok on submit"])
    end

    it "still uses the purpose tag for other purposes" do
      result = contract.(name: "nope", purposes: :draft)

      expect(result.rule_selection.entry_for(:name).reason).to be(:purpose)
    end
  end

  describe "introspection" do
    it "exposes why every applied rule was selected" do
      result = contract.(name: "ab", summary: "x", details: "de", purposes: :draft)

      reasons = result.rule_selection.applied.to_h { |entry| [entry.key, entry.reason] }

      expect(reasons).to eql(name: :base, details: :dependency, summary: :purpose)
    end

    it "marks omitted rules with the purpose reason" do
      result = contract.(name: "ab", purposes: :submit)

      expect(result.rule_selection.omitted.map(&:reason)).to eql(%i[purpose])
      expect(result.rule_selection).to be_omitted(:summary)
      expect(result.rule_selection).not_to be_applies(:summary)
    end
  end

  describe "compilation caching" do
    it "does not leak a selection between different combinations" do
      submit = contract.(name: "ab", purposes: :submit)
      draft = contract.(name: "ab", purposes: :draft)
      submit_again = contract.(name: "ab", purposes: :submit)

      expect(draft.rule_selection.applied_keys).not_to include(:consent)
      expect(submit.rule_selection.applied_keys).to include(:consent)
      expect(submit_again.rule_selection).to equal(submit.rule_selection)
    end

    it "keeps a per-instance cache" do
      other = contract_class.new

      contract.(name: "ab", purposes: :submit)

      expect(other.call(name: "ab").rule_selection.applied_keys).to eql(%i[name])
    end
  end

  describe "inheritance" do
    let(:parent_class) do
      Class.new(Dry::Validation::Contract) do
        params { required(:name).filled(:string); optional(:consent).maybe(:bool) }

        rule(:name) { key.failure("base") if value.to_s.empty? }
        rule(:consent).for(:submit) { key.failure("need consent") unless value == true }
      end
    end

    let(:child_class) do
      Class.new(parent_class) do
        params { required(:name).filled(:string); optional(:consent).maybe(:bool) }

        rule(:name).for(:archive) { key.failure("archive-only") if value == "archive" }
      end
    end

    it "does not mutate the parent when a child adds a new purpose" do
      child_class

      expect(parent_class.rules.flat_map(&:purposes)).not_to include(:archive)
      expect(parent_class.rules.size).to be(2)
      expect(child_class.rules.size).to be(3)
    end

    it "keeps parent and child selections independent" do
      parent_result = parent_class.new.(name: "ab", purposes: :archive)
      child_result = child_class.new.(name: "archive", purposes: :archive)

      expect(parent_result.rule_selection.applied_keys).to eql(%i[name])
      expect(child_result.errors.to_h).to eql(name: ["archive-only"])

      valid_child = child_class.new.(name: "ab", purposes: :archive)
      expect(valid_child.errors.to_h).to eql({})
    end
  end
end
