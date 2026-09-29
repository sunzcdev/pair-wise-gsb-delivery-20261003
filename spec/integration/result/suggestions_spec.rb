# frozen_string_literal: true

RSpec.describe Dry::Validation::Result, "suggestions" do
  subject(:contract) do
    Class.new(Dry::Validation::Contract) do
      params do
        required(:status).filled(:string)
        optional(:legacy_id).maybe(:integer)
        optional(:tags).array(:string)
        optional(:address).hash do
          required(:city).filled(:string)
        end
      end

      rule(:status) do
        unless %w[active archived].include?(value)
          key.failure("is invalid", {}, [
            {operation: :replace, path: [:status], value: "active"},
            {operation: :replace, path: [:status], value: "archived"}
          ])
        end
      end

      rule(:legacy_id) do
        if key?
          key.failure(
            {text: "is deprecated",
             suggestions: [Dry::Validation::Suggestion.delete([:legacy_id])]}
          )
        end
      end
    end.new
  end

  describe "#suggestions" do
    it "collects suggestions from rule errors and keeps the error reference" do
      result = contract.(status: "unknown", legacy_id: 1)

      expect(result.suggestions.size).to be(3)
      expect(result.suggestions.map(&:error)).to all(be_a(Dry::Validation::Message))
      expect(result.suggestions.map(&:error).map(&:text)).to eql(
        ["is invalid", "is invalid", "is deprecated"]
      )
    end

    it "returns an empty array when there are no suggestions" do
      expect(contract.(status: "active").suggestions).to be_empty
    end
  end

  describe "#compatible_suggestions / #conflicting_suggestions" do
    it "separates mergeable suggestions from mutually exclusive ones" do
      result = contract.(status: "unknown", legacy_id: 1)

      expect(result.compatible_suggestions).to eql(
        [Dry::Validation::Suggestion.delete([:legacy_id])]
      )
      expect(result.conflicting_suggestions).to eql(
        [
          Dry::Validation::Suggestion.replace([:status], "active"),
          Dry::Validation::Suggestion.replace([:status], "archived")
        ]
      )
      expect(result.conflict_groups).to eql([result.conflicting_suggestions])
    end
  end

  describe "#apply_suggestions" do
    it "applies the selected suggestions to a copy of the input and re-validates once" do
      input = {status: "unknown", legacy_id: 1}
      result = contract.(input)

      fixed = result.apply_suggestions(
        [result.suggestions[0], result.suggestions[2]]
      )

      expect(fixed).to be_success
      expect(fixed.to_h).to eql({status: "active"})
      expect(input).to eql({status: "unknown", legacy_id: 1})
      expect(result.errors.to_h).to eql(
        {status: ["is invalid"], legacy_id: ["is deprecated"]}
      )
    end

    it "defaults to the compatible set" do
      result = contract.(status: "unknown", legacy_id: 1)

      fixed = result.apply_suggestions

      expect(fixed.to_h).to eql({status: "unknown"})
      expect(fixed.errors.to_h).to eql({status: ["is invalid"]})
    end

    it "never applies suggestions automatically in a loop" do
      result = contract.(status: "unknown", legacy_id: 1)

      fixed = result.apply_suggestions

      expect(fixed).to be_failure
      expect(fixed.suggestions.size).to be(2)
      expect(fixed.to_h).to eql({status: "unknown"})
    end

    it "raises when the selection contains conflicting suggestions" do
      result = contract.(status: "unknown")

      expect { result.apply_suggestions(result.suggestions) }.to raise_error(
        Dry::Validation::ConflictingSuggestionsError, /conflicts with/
      )
    end

    it "raises when a suggestion did not come from the result" do
      result = contract.(status: "unknown")

      expect {
        result.apply_suggestions([Dry::Validation::Suggestion.delete([:legacy_id])])
      }.to raise_error(Dry::Validation::InvalidSuggestionError)
    end
  end
end
