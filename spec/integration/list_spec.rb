# frozen_string_literal: true

# A business error without a `+` operator: it carries a path and a severity.
class ListValidationFieldError < Data.define(:path, :severity); end

# Accumulator used by the custom Validated merge rule: keeps every error
# (duplicate paths included) and tallies severities separately.
class ListValidationErrorReport
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

RSpec.describe(Dry::Monads::List) do
  list = described_class

  context "mapping a block" do
    it "maps a block over list values" do
      expect(list[1, 2, 3].fmap { _1 + 1 }).to eql(list[2, 3, 4])
    end
  end

  context "binding a block" do
    it "binds a block" do
      expect(list[1, 2, 3].bind { [_1 + 1, _1 + 2] }).to eql(list[2, 3, 3, 4, 4, 5])
    end
  end

  describe(described_class::Mixin) do
    subject { Class.new { include Dry::Monads::List::Mixin } }

    it "adds List constructor" do
      expect(subject.new.List([1])).to eql(list[1])
    end
  end

  describe list::Validated do
    include Dry::Monads::Validated::Mixin

    it "traverses errors" do
      errors = list::Validated[
        Invalid(:no_email), Invalid(:no_name), Valid(age: 29)
      ]

      expect(errors.traverse).to eql(Invalid(list[:no_email, :no_name]))
    end

    it "traverses valids" do
      errors = list::Validated[
        Valid("john@doe.me"), Valid("John")
      ]

      expect(errors.traverse).to eql(Valid(list["john@doe.me", "John"]))
    end

    context "with an explicit error merge rule" do
      let(:rule) { Dry::Monads::Validated.rule("field_errors", &:merge) }

      def report(error) = ListValidationErrorReport.single(error)

      it "accumulates errors with the rule" do
        email = ListValidationFieldError.new("email", :error)
        name = ListValidationFieldError.new("name", :warning)

        result = list::Validated[
          rule.invalid(report(email)), rule.invalid(report(name))
        ].traverse

        expected_report = report(email).merge(report(name))
        expect(result).to eql(Dry::Monads::Invalid.new(expected_report))
        expect(result.rule).to equal(rule)
        expect(result.error.entries).to eql([email, name])
        expect(result.error.severities).to eql(error: 1, warning: 1)
      end

      it "keeps duplicate paths while counting severities separately" do
        errors = [
          ListValidationFieldError.new("name", :error),
          ListValidationFieldError.new("name", :error),
          ListValidationFieldError.new("age", :warning)
        ]

        sequential = list::Validated[
          *errors.map { rule.invalid(report(_1)) }
        ].traverse

        grouped = list::Validated[
          rule.invalid(report(errors[0])),
          rule.invalid(report(errors[1]).merge(report(errors[2])))
        ].traverse

        expect(sequential.error.entries).to eql(errors)
        expect(grouped.error.entries).to eql(errors)
        expect(sequential.error).to eql(grouped.error)
        expect(sequential.error.severities).to eql(error: 2, warning: 1)
      end

      it "refuses to traverse values carrying different rules" do
        other = Dry::Monads::Validated.rule("other", &:merge)

        expect {
          list::Validated[
            rule.invalid(report(ListValidationFieldError.new("a", :error))),
            other.invalid(report(ListValidationFieldError.new("b", :error)))
          ].traverse
        }.to raise_error(Dry::Monads::ValidatedRuleMismatchError, /different error merge rules/)
      end
    end
  end
end
