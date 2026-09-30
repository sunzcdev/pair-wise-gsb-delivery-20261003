# frozen_string_literal: true

RSpec.describe(Dry::Monads::Validated) do
  validated = described_class
  valid = described_class::Valid.method(:new)
  invalid = described_class::Invalid.method(:new)
  maybe = Dry::Monads::Maybe
  some = maybe::Some.method(:new)
  unit = Dry::Monads::Unit

  result = Dry::Monads::Result
  success = result::Success.method(:new)
  failure = result::Failure.method(:new)

  it_behaves_like "an applicative" do
    let(:pure) { valid }
  end

  describe ".pure" do
    it "constructs a Valid value" do
      expect(validated.pure(1)).to eql(valid.(1))
    end

    it "accepts a block" do
      fn = -> x { x }
      expect(validated.pure(&fn)).to eql(valid.(fn))
    end
  end

  describe validated::Valid do
    subject { valid.(1) }

    it_behaves_like "a constructor"
    it_behaves_like "a functor"

    describe "#inspect" do
      it "returns the string representation" do
        expect(subject.inspect).to eql("Valid(1)")
        expect(valid[unit].inspect).to eql("Valid()")
      end
    end

    describe "#fmap" do
      it "lifts a block" do
        expect(subject.fmap { (_1 + 1).to_s }).to eql(valid.("2"))
      end
    end

    describe "#value!" do
      it "extracts the stored value" do
        expect(subject.value!).to eql(1)
      end
    end

    describe "#alt_map" do
      it "is an inversed fmap" do
        expect(subject.alt_map { raise }).to be(subject)
        expect(subject.alt_map(-> { raise })).to be(subject)
      end
    end

    describe "#or" do
      it "returns self back" do
        expect(subject.or { raise }).to be(subject)
        expect(subject.or(-> { raise })).to be(subject)
      end
    end

    describe "#to_maybe" do
      it "returns Some" do
        expect(subject.to_maybe).to eql(some.(1))
      end
    end

    describe "#to_result" do
      it "retuns Success" do
        expect(subject.to_result).to eql(success.(1))
      end
    end

    describe "#apply" do
      subject { valid.(-> x { x + 1 }) }

      it "applies the function to a valid value" do
        expect(subject.apply(valid.(2))).to eql(valid.(3))
      end

      it "returns invalid back" do
        expect(subject.apply(invalid.(2))).to eql(invalid.(2))
      end
    end

    describe "#===" do
      it "matches on the wrapped value" do
        expect(valid["foo"]).to be === valid["foo"]
        expect(valid[/\w+/]).to be === valid["foo"]
        expect(valid[:bar]).not_to be === valid["foo"]
        expect(valid[10..50]).to be === valid[42]
      end
    end
  end

  describe validated::Invalid do
    subject { invalid.(:missing_value) }

    it_behaves_like "a constructor"

    describe "#inspect" do
      it "returns the string representation" do
        expect(subject.inspect).to eql("Invalid(:missing_value)")
      end
    end

    describe "#fmap" do
      it "returns self back" do
        expect(subject.fmap { raise }).to be(subject)
        expect(subject.fmap(-> { raise })).to be(subject)
      end
    end

    describe "#alt_map" do
      it "is an inversed fmap" do
        expect(subject.alt_map(&:to_s)).to eql(invalid.("missing_value"))
        expect(subject.alt_map(-> value { value.to_s })).to eql(invalid.("missing_value"))
      end

      it "traces the caller" do
        expect(subject.alt_map { _1 }.trace).to include("validated_spec.rb")
      end
    end

    describe "#error" do
      it "returns the stored value" do
        expect(subject.error).to eql(:missing_value)
      end
    end

    describe "#or" do
      it "yields a block" do
        expect(subject.or { :result }).to eql(:result)
        expect(subject.or(-> { :result })).to eql(:result)
      end
    end

    describe "#apply" do
      it "concatenates errors using +" do
        expect(invalid.(1).apply(invalid.(2))).to eql(invalid.(3))
      end
    end

    describe "#to_maybe" do
      it "returns None" do
        expect(subject.to_maybe).to be_none
      end

      it "traces the caller" do
        expect(subject.to_maybe.trace).to include("spec/unit/validated_spec.rb")
      end
    end

    describe "#to_result" do
      it "retuns Failure" do
        expect(subject.to_result).to eql(failure.(:missing_value))
      end

      it "traces the caller" do
        expect(subject.to_result.trace).to include("spec/unit/validated_spec.rb")
      end
    end

    describe "#===" do
      it "matches on the wrapped value" do
        expect(invalid["foo"]).to be === invalid["foo"]
        expect(invalid[/\w+/]).to be === invalid["foo"]
        expect(invalid[:bar]).not_to be === invalid["foo"]
        expect(invalid[10..50]).to be === invalid[42]
      end
    end
  end

  describe "#bind" do
    it "tells that Validated has no monad instance" do
      expect {
        valid.(1).bind { raise }
      }.to raise_error(
        NotImplementedError,
        "Validated is not a monad because it would violate the monad laws"
      )
    end
  end

  describe "error merge rules" do
    include Dry::Monads::Validated::Mixin

    # Business error objects with a path and a severity.
    # They deliberately don't implement +
    field_error = Struct.new(:path, :severity)

    # A merge rule that keeps duplicate paths and
    # tallies severities separately
    tally_merge = lambda do |left, right|
      to_tally = nil
      to_tally = lambda do |errors|
        case errors
        when Hash then errors
        when field_error then to_tally.call([errors])
        else
          errors.to_ary.each_with_object({}) do |error, tally|
            tally[error.path] ||= Hash.new(0)
            tally[error.path][error.severity] += 1
          end
        end
      end

      to_tally.call(left).merge(to_tally.call(right)) do |_path, left_severities, right_severities|
        left_severities.merge(right_severities) do |_severity, left_count, right_count|
          left_count + right_count
        end
      end
    end

    name_error = field_error.new(:name, :error)
    name_warning = field_error.new(:name, :warning)
    email_error = field_error.new(:email, :error)
    another_name_error = field_error.new(:name, :error)

    # duplicate paths are kept, severities are counted separately
    expected_tally = {
      name: {error: 2, warning: 1},
      email: {error: 1}
    }

    it "combines errors with the rule instead of +" do
      merged = Invalid(name_error, tally_merge).apply(Invalid(name_warning, tally_merge))

      expect(merged).to eql(Invalid({name: {error: 1, warning: 1}}, tally_merge))
    end

    it "attaches the rule via .pure" do
      expect(validated.pure(1, tally_merge).merge_rule).to be(tally_merge)
    end

    it "keeps the rule after apply" do
      merged = Invalid(name_error, tally_merge).apply(Invalid(email_error, tally_merge))

      expect(merged.merge_rule).to be(tally_merge)
    end

    it "keeps the rule when a Valid function is applied to an Invalid" do
      merged = Valid(-> x { x }).apply(Invalid(name_error, tally_merge))

      expect(merged.merge_rule).to be(tally_merge)
    end

    it "keeps the rule after alt_map" do
      expect(Invalid(name_error, tally_merge).alt_map { [_1] }.merge_rule).to be(tally_merge)
    end

    it "keeps the rule after fmap on Valid" do
      expect(Valid(1, tally_merge).fmap { _1 + 1 }.merge_rule).to be(tally_merge)
    end

    it "adopts the rule from either side when the other one has none" do
      expect(
        Invalid(name_error, tally_merge).apply(Invalid(email_error)).merge_rule
      ).to be(tally_merge)

      expect(
        Invalid(name_error).apply(Invalid(email_error, tally_merge)).merge_rule
      ).to be(tally_merge)
    end

    it "uses the adopted rule for combining" do
      merged = Invalid(name_error).apply(Invalid(email_error, tally_merge))

      expect(merged.error).to eql(name: {error: 1}, email: {error: 1})
    end

    it "rejects combining results with different rules" do
      other_rule = ->(a, _b) { a }

      expect {
        Invalid(name_error, tally_merge).apply(Invalid(email_error, other_rule))
      }.to raise_error(Dry::Monads::MergeRuleMismatchError, /different error merge rules/)
    end

    it "produces the same result for sequential and grouped combination" do
      sequential = Invalid(name_error, tally_merge)
        .apply(Invalid(name_warning, tally_merge))
        .apply(Invalid(email_error, tally_merge))
        .apply(Invalid(another_name_error, tally_merge))

      grouped = Invalid(name_error, tally_merge)
        .apply(Invalid(name_warning, tally_merge))
        .apply(
          Invalid(email_error, tally_merge).apply(Invalid(another_name_error, tally_merge))
        )

      expect(sequential.error).to eql(expected_tally)
      expect(grouped).to eql(sequential)
    end

    describe "List#traverse" do
      list_validated = Dry::Monads::List::Validated

      it "keeps the rule and combines errors with it" do
        results = list_validated[
          Invalid(name_error, tally_merge),
          Invalid(name_warning, tally_merge),
          Valid("ok"),
          Invalid(email_error, tally_merge),
          Invalid(another_name_error, tally_merge)
        ]

        traversed = results.traverse

        expect(traversed.error).to eql(expected_tally)
        expect(traversed.merge_rule).to be(tally_merge)
      end

      it "matches sequential combination" do
        sequential = Invalid(name_error, tally_merge)
          .apply(Invalid(name_warning, tally_merge))
          .apply(Invalid(email_error, tally_merge))
          .apply(Invalid(another_name_error, tally_merge))

        traversed = list_validated[
          Invalid(name_error, tally_merge),
          Invalid(name_warning, tally_merge),
          Invalid(email_error, tally_merge),
          Invalid(another_name_error, tally_merge)
        ].traverse

        expect(traversed.error).to eql(sequential.error)
      end

      it "rejects mixed rules during traversal" do
        results = list_validated[
          Invalid(name_error, tally_merge),
          Invalid(email_error, ->(a, _b) { a })
        ]

        expect { results.traverse }.to raise_error(Dry::Monads::MergeRuleMismatchError)
      end
    end
  end
end
