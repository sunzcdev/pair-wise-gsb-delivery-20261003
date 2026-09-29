# frozen_string_literal: true

require "dry/validation/contract"

RSpec.describe Dry::Validation::RuleCompiler do
  def rule(*keys, **options, &block)
    block ||= proc {}
    Dry::Validation::Rule.new(keys: keys, block: block, **options)
  end

  subject(:compiler) { described_class.new(rules) }

  describe "#compile" do
    let(:base_rule) { rule(:name) }
    let(:submit_rule) { rule(:consent, purposes: %i[submit]) }
    let(:draft_rule) { rule(:summary, purposes: %i[draft]) }

    let(:rules) { [base_rule, submit_rule, draft_rule] }

    it "keeps only base rules without purposes" do
      selection = compiler.compile

      expect(selection.applied_keys).to eql(%i[name])
      expect(selection.omitted_keys).to eql(%i[consent summary])
    end

    it "selects rules tagged with a requested purpose" do
      selection = compiler.compile(:submit)

      expect(selection.applied_keys).to eql(%i[name consent])
      expect(selection.applied.map(&:reason)).to eql(%i[base purpose])
    end

    it "unions multiple purposes" do
      selection = compiler.compile(:submit, :draft)

      expect(selection.applied_keys).to eql(%i[name consent summary])
      expect(selection.omitted_keys).to eql([])
    end

    it "returns independent selections for different combinations" do
      submit = compiler.compile(:submit)
      draft = compiler.compile(:draft)

      expect(submit.applied_keys).not_to eql(draft.applied_keys)
      expect(submit.purposes).to eql(%i[submit])
      expect(draft.purposes).to eql(%i[draft])
    end

    context "with transitive dependencies" do
      let(:dependency) { rule(:details, purposes: %i[submit]) }
      let(:dependent) { rule(:summary, purposes: %i[draft], depends_on: %i[details]) }

      let(:rules) { [base_rule, dependency, dependent] }

      it "pulls dependencies in and orders them first" do
        selection = compiler.compile(:draft)

        expect(selection.applied_keys).to eql(%i[name details summary])
        expect(selection.entry_for(:details).reason).to be(:dependency)
        expect(selection.entry_for(:details).source).to be(:summary)
      end
    end

    context "with mandatory rules" do
      let(:mandatory_rule) { rule(:summary, purposes: %i[draft], mandatory: %i[submit]) }
      let(:rules) { [base_rule, mandatory_rule] }

      it "forces the rule for the declared purpose" do
        selection = compiler.compile(:submit)

        expect(selection.applied_keys).to eql(%i[name summary])
        expect(selection.entry_for(:summary).reason).to be(:mandatory)
      end
    end

    context "with an unknown dependency" do
      let(:rules) { [rule(:name, depends_on: %i[ghost])] }

      it "raises an InvalidDependencyError" do
        expect { compiler.compile }.to raise_error(Dry::Validation::InvalidDependencyError)
      end
    end

    context "with a dependency cycle" do
      let(:rules) do
        [
          rule(:a, purposes: %i[submit], depends_on: %i[b]),
          rule(:b, purposes: %i[submit], depends_on: %i[a])
        ]
      end

      it "raises a CircularDependencyError" do
        expect { compiler.compile(:submit) }.to raise_error(Dry::Validation::CircularDependencyError)
      end
    end
  end
end
