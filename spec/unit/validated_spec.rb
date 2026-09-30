# frozen_string_literal: true

# A business error that deliberately has no `+` semantics:
# it carries a path and a severity.
ValidatedFieldError = Data.define(:path, :severity)

# Accumulator used by custom merge rules: keeps every error (duplicate
# paths included) and tallies severities separately.
class ValidatedErrorReport
  attr_reader :entries, :severities

  def initialize(entries = [], severities = Hash.new(0))
    @entries = entries
    @severities = severities
  end

  def self.single(error)
    new([error], error.severity => 1)
  end

  def merge(other)
    self.class.new(
      entries + other.entries,
      severities.merge(other.severities) { |_, a, b| a + b }
    )
  end

  def ==(other)
    other.is_a?(self.class) &&
      entries == other.entries &&
      severities == other.severities
  end
  alias_method :eql?, :==

  def hash = [entries, severities].hash
end

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

  describe "explicit error merge rules" do
    # Note: no `wrap` given, so a raw error passed to Invalid is expected
    # to already be a Report; the rule just defines how reports combine.
    let(:validated) { described_class }

    let(:rule) do
      validated.rule("report", &:merge)
    end

    let(:email) { ValidatedFieldError.new("user.email", :error) }
    let(:name) { ValidatedFieldError.new("user.name", :warning) }
    let(:name_dup) { ValidatedFieldError.new("user.name", :error) }

    describe described_class::Rule do
      let(:validated) { Dry::Monads::Validated }

      describe ".default?" do
        it "is true only for the default rule" do
          expect(validated::Rule.default?(validated::Rule::DEFAULT)).to be(true)
          expect(validated::Rule.default?(rule)).to be(false)
        end
      end

      describe "#invalid and #valid" do
        it "attach the rule to constructed values" do
          expect(rule.invalid(ValidatedErrorReport.single(email)).rule).to equal(rule)
          expect(rule.valid(1).rule).to equal(rule)
        end
      end

      describe "#inspect" do
        it "uses the given name" do
          expect(rule.inspect).to eql("Dry::Monads::Validated::Rule(report)")
        end
      end
    end

    describe "apply" do
      it "combines errors with the rule instead of calling +" do
        result = rule.invalid(ValidatedErrorReport.single(email))
          .apply(rule.invalid(ValidatedErrorReport.single(name)))

        expect(result).to eql(invalid.(ValidatedErrorReport.single(email).merge(ValidatedErrorReport.single(name))))
        expect(result.rule).to equal(rule)
      end

      it "gives the same result for sequential and grouped composition" do
        a = rule.invalid(ValidatedErrorReport.single(email))
        b = rule.invalid(ValidatedErrorReport.single(name))
        c = rule.invalid(ValidatedErrorReport.single(name_dup))

        sequential = a.apply(b).apply(c)
        grouped = a.apply(b.apply(c))
        expected_entries = [email, name, name_dup]

        expect(sequential.error.entries).to eql(expected_entries)
        expect(grouped.error.entries).to eql(expected_entries)
        expect(sequential.error).to eql(grouped.error)

        # duplicate paths are kept, severities are tallied separately
        expect(sequential.error.severities).to eql(error: 2, warning: 1)
        expect(grouped.error.severities).to eql(error: 2, warning: 1)
      end

      it "keeps the rule when a valid value is applied" do
        result = rule.invalid(ValidatedErrorReport.single(email)).apply(valid.(1))
        expect(result).to eql(invalid.(ValidatedErrorReport.single(email)))
        expect(result.rule).to equal(rule)
      end

      it "propagates the rule through valid sides of apply" do
        result = valid.(-> x { x }).apply(rule.invalid(ValidatedErrorReport.single(email)))
        expect(result).to eql(invalid.(ValidatedErrorReport.single(email)))
        expect(result.rule).to equal(rule)
      end

      it "rejects results carrying different explicit rules" do
        other = described_class.rule("other", &:merge)

        expect {
          rule.invalid(ValidatedErrorReport.single(email)).apply(other.invalid(ValidatedErrorReport.single(name)))
        }.to raise_error(
          Dry::Monads::ValidatedRuleMismatchError,
          /different error merge rules/
        )
      end

      it "lets the default rule yield to an explicit one" do
        combined = invalid.(ValidatedErrorReport.single(email))
          .apply(rule.invalid(ValidatedErrorReport.single(name)))

        expect(combined.error).to eql(ValidatedErrorReport.single(email).merge(ValidatedErrorReport.single(name)))
        expect(combined.rule).to equal(rule)
      end
    end

    describe "alt_map" do
      it "preserves the rule when transforming the error" do
        augmented = ->(report) { report.merge(ValidatedErrorReport.single(name)) }

        result = rule.invalid(ValidatedErrorReport.single(email)).alt_map(&augmented)

        expect(result).to eql(invalid.(ValidatedErrorReport.single(email).merge(ValidatedErrorReport.single(name))))
        expect(result.rule).to equal(rule)
      end
    end

    describe "List#traverse" do
      let(:list) { Dry::Monads::List }

      it "uses the rule carried by invalid elements and accumulates every error" do
        traversed = list::Validated[
          rule.invalid(ValidatedErrorReport.single(email)),
          rule.invalid(ValidatedErrorReport.single(name)),
          rule.invalid(ValidatedErrorReport.single(name_dup))
        ].traverse

        expect(traversed).to eql(
          invalid.(
            ValidatedErrorReport.single(email)
              .merge(ValidatedErrorReport.single(name))
              .merge(ValidatedErrorReport.single(name_dup))
          )
        )
        expect(traversed.rule).to equal(rule)
        expect(traversed.error.entries).to eql([email, name, name_dup])
        expect(traversed.error.severities).to eql(error: 2, warning: 1)
      end

      it "preserves the rule across a subsequent traverse-like combination" do
        first = list::Validated[
          rule.invalid(ValidatedErrorReport.single(email)),
          rule.invalid(ValidatedErrorReport.single(name))
        ].traverse

        second = list::Validated[
          rule.invalid(ValidatedErrorReport.single(name_dup))
        ].traverse

        result = first.apply(second)
        expect(result.error.entries).to eql([email, name, name_dup])
        expect(result.rule).to equal(rule)
      end

      it "rejects traversal with mismatched rules" do
        other = described_class.rule("other", &:merge)

        expect {
          list::Validated[
            rule.invalid(ValidatedErrorReport.single(email)),
            other.invalid(ValidatedErrorReport.single(name))
          ].traverse
        }.to raise_error(Dry::Monads::ValidatedRuleMismatchError)
      end
    end

    describe "rules with a lift wrapper" do
      let(:rule) do
        described_class.rule("report", wrap: ValidatedErrorReport.method(:single), &:merge)
      end

      it "wraps raw errors with lift during traverse" do
        traversed = Dry::Monads::List::Validated[
          rule.invalid(email),
          rule.invalid(name)
        ].traverse

        expect(traversed.error.entries).to eql([email, name])
        expect(traversed.error.severities).to eql(error: 1, warning: 1)
        expect(traversed.rule).to equal(rule)
      end
    end
  end
end
