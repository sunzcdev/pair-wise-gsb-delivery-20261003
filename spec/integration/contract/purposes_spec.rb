# frozen_string_literal: true

require "dry/validation/contract"

RSpec.describe Dry::Validation::Contract, "rule purposes" do
  subject(:contract) { contract_class.new }

  let(:contract_class) do
    Class.new(Dry::Validation::Contract) do
      def self.name
        "ApplicationContract"
      end

      purposes :draft
      purposes :submit, includes: :terms
      purposes :terms

      params do
        required(:name).filled(:string)
        optional(:terms).maybe(:bool)
        optional(:lines).array(:integer)
        optional(:total).maybe(:integer)
      end

      rule(:name) do
        key.failure("too short") if value.length < 3
      end

      rule(:terms, purposes: :terms) do
        key.failure("must accept terms") if key? && values[:terms] != true
      end

      rule(:lines, name: :line_items, purposes: :submit) do
        key.failure("must have lines") if key? && values[:lines].empty?
      end

      rule(:total, purposes: :submit, depends_on: :line_items) do
        key.failure("must be non-negative") if key? && !values[:total].nil? && values[:total] < 0
      end
    end
  end

  let(:valid_input) { {name: "Jane", terms: true, lines: [1], total: 1} }

  describe "selecting rules by purpose" do
    it "runs base rules only for the draft purpose" do
      result = contract.call(valid_input.merge(terms: false, total: -1), purposes: :draft)

      expect(result).to be_success
      expect(result.rules_applied.map(&:id)).to eql(%i[name])
      expect(result.rules_omitted.map(&:id)).to eql(%i[terms line_items total])
    end

    it "applies purposes pulled in through includes when submitting" do
      result = contract.call(valid_input.merge(terms: false), purposes: :submit)

      expect(result.errors.to_h).to eql(terms: ["must accept terms"])
      expect(result.rule_selection.effective_purposes).to eql(%i[submit terms])
    end

    it "accepts several purposes at once" do
      result = contract.call(valid_input, purposes: %i[draft submit])

      expect(result).to be_success
      expect(result.rules_applied.map(&:id)).to eql(%i[name terms line_items total])
    end

    it "runs every rule when no purpose is requested" do
      result = contract.call(valid_input)

      expect(result).to be_success
      expect(result.rules_applied.map(&:id)).to eql(%i[name terms line_items total])
      expect(result.rules_omitted).to be_empty
    end
  end

  describe "mandatory rules via includes" do
    it "marks rules selected because their included purpose matched" do
      result = contract.call(valid_input.merge(terms: false), purposes: :submit)

      entry = result.rule_selection.entry_for(:terms)
      expect(entry).not_to be_nil
      expect(entry.reason).to be(:purpose)
      expect(entry.purposes).to eql(%i[terms])
    end
  end

  describe "cross-purpose dependencies" do
    subject(:contract) { cross_class.new }

    let(:cross_class) do
      Class.new(Dry::Validation::Contract) do
        def self.name
          "CrossPurposeContract"
        end

        purposes :draft, :submit

        params do
          required(:prerequisite).filled(:string)
          required(:confirmation).filled(:string)
        end

        rule(:prerequisite, purposes: :submit) do
          key.failure("prerequisite missing") if value == "bad"
        end

        rule(:confirmation, purposes: :draft, depends_on: :prerequisite) do
          key.failure("confirmation mismatch") if value == "bad"
        end
      end
    end

    it "pulls a dependency in even though its purpose was not requested" do
      result = contract.call({prerequisite: "bad", confirmation: "ok"}, purposes: :draft)

      expect(result.errors.to_h).to eql(prerequisite: ["prerequisite missing"])
      expect(result.rules_applied.map(&:id)).to eql(%i[prerequisite confirmation])

      entry = result.rule_selection.entry_for(:prerequisite)
      expect(entry.reason).to be(:dependency)
      expect(entry.required_by).to eql(%i[confirmation])
    end

    it "orders dependencies before dependent rules regardless of declaration order" do
      executed = []

      ordered_class = Class.new(Dry::Validation::Contract) do
        purposes :draft, :submit

        params do
          required(:prerequisite).filled(:string)
          required(:confirmation).filled(:string)
        end

        # Dependent rule is declared first, on purpose.
        rule(:confirmation, purposes: :draft, depends_on: :prerequisite) do
          executed << :confirmation
        end

        rule(:prerequisite, purposes: :submit) do
          executed << :prerequisite
        end
      end

      ordered_class.new.call({prerequisite: "ok", confirmation: "ok"}, purposes: :draft)

      expect(executed).to eql(%i[prerequisite confirmation])
    end
  end

  describe "result introspection" do
    it "reports selected and omitted rules together with reasons" do
      result = contract.call(valid_input, purposes: :draft)
      selection = result.rule_selection

      expect(selection.requested_purposes).to eql(%i[draft])
      expect(selection.selected_entries.map(&:name)).to eql(%i[name])
      expect(selection.omitted.map(&:name)).to eql(%i[terms line_items total])
      expect(selection.omitted.map(&:reason)).to all(eq(:omitted))
      expect(selection).to be_selected(:name)
      expect(selection).not_to be_selected(:total)
    end

    it "enumerates selected entries" do
      result = contract.call(valid_input, purposes: :draft)

      expect(result.rule_selection.map(&:name)).to eql(%i[name])
    end
  end

  describe "compilation cache" do
    it "reuses the compiled selection for repeated calls with the same combination" do
      first = contract.call(valid_input, purposes: :draft).rule_selection
      second = contract.call(valid_input, purposes: :draft).rule_selection

      expect(first).to equal(second)
    end

    it "keeps distinct combinations isolated" do
      draft = contract.call(valid_input, purposes: :draft).rule_selection
      submit = contract.call(valid_input, purposes: :submit).rule_selection

      expect(draft).not_to equal(submit)
      expect(draft.rules.map(&:id)).not_to eql(submit.rules.map(&:id))
    end
  end

  describe "inheritance" do
    let(:parent_class) do
      Class.new(Dry::Validation::Contract) do
        def self.name
          "ParentContract"
        end

        purposes :draft

        params do
          required(:email).filled(:string)
        end

        rule(:email, purposes: :draft) {}
      end
    end

    it "does not leak purposes registered in the child back to the parent" do
      child = Class.new(parent_class) { purposes :submit }

      expect(child.registered_purposes).to eql(%i[draft submit])
      expect(parent_class.registered_purposes).to eql(%i[draft])
    end

    it "inherits parent purposes and keeps rule selections isolated per class" do
      child = Class.new(parent_class) do
        def self.name
          "ChildContract"
        end

        purposes :submit

        params do
          required(:name).filled(:string)
        end

        rule(:name, purposes: :submit) {}
      end

      parent_result = parent_class.new.call({email: "a@b.c"}, purposes: :draft)
      child_result = child.new.call({email: "a@b.c", name: "Jane"}, purposes: :submit)

      expect(parent_result.rules_applied.map(&:id)).to eql(%i[email])
      expect(child_result.rules_applied.map(&:id)).to eql(%i[name])
      expect(child_result.rules_omitted.map(&:id)).to eql(%i[email])
    end
  end

  describe "compile-time errors" do
    it "raises for an unknown requested purpose" do
      expect { contract.call(valid_input, purposes: :nope) }
        .to raise_error(Dry::Validation::UnknownPurposeError, /unknown purposes: \[:nope\]/)
    end

    it "raises when a rule references an unregistered purpose" do
      contract_class.rule(:name, purposes: :nope) {}

      expect { contract.call(valid_input) }
        .to raise_error(Dry::Validation::UnknownPurposeError, /unregistered purposes/)
    end

    it "raises for a missing rule dependency" do
      contract_class.rule(:name, depends_on: :ghost) {}

      expect { contract.call(valid_input, purposes: :draft) }
        .to raise_error(Dry::Validation::UnknownRuleDependencyError, /:ghost/)
    end

    it "raises for circular rule dependencies" do
      contract_class.rule(:lines, name: :a, depends_on: :b) {}
      contract_class.rule(:total, name: :b, depends_on: :a) {}

      expect { contract.call(valid_input, purposes: :submit) }
        .to raise_error(Dry::Validation::CircularRuleDependencyError, /circular/)
    end
  end
end
