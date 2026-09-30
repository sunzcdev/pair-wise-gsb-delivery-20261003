# frozen_string_literal: true

RSpec.describe(Dry::Monads::LazyList) do
  lazy_list = described_class

  it "is available when all monads are loaded" do
    combinations = lazy_list.new([1, 2]).bind do |x|
      lazy_list.new([10, 20]).fmap { |y| [x, y] }
    end

    expect(combinations.to_a).to eql([[1, 10], [1, 20], [2, 10], [2, 20]])
  end

  it "is available with cherry-picked loading" do
    mixin = Dry::Monads[:lazy_list]

    expect(mixin.const_get(:LazyList)).to be(lazy_list)
  end
end
