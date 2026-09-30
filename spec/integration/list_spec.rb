# frozen_string_literal: true

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

    it "traverses errors with a custom merge rule" do
      merge = ->(a, b) { a.merge(b) { |_key, errs_a, errs_b| errs_a + errs_b } }

      errors = list::Validated[
        Invalid({name: ["is missing"]}, merge),
        Valid(age: 29),
        Invalid({email: ["is missing"]}, merge)
      ]

      expect(errors.traverse { |el| el }).to eql(
        Invalid({name: ["is missing"], email: ["is missing"]}, merge)
      )
    end

    it "rejects mixed merge rules during traversal" do
      errors = list::Validated[
        Invalid({name: ["is missing"]}, ->(a, b) { a.merge(b) }),
        Invalid({email: ["is missing"]}, ->(a, _b) { a })
      ]

      expect { errors.traverse { |el| el } }.to raise_error(
        Dry::Monads::MergeRuleMismatchError
      )
    end
  end
end
