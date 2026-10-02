# frozen_string_literal: true

module RecursiveInferTypes
  include Dry.Types()
end

RSpec.describe "recursive type inferrers" do
  let(:tree) do
    Dry::Types.recursive(:x) do |r|
      RecursiveInferTypes::Integer | RecursiveInferTypes::Array.of(r[:x])
    end
  end

  it "infers primitives without recursing forever" do
    expect(Dry::Types::PrimitiveInferrer.new.[](tree)).to eql([Integer, Array])
  end

  it "infers predicates without recursing forever" do
    expect(Dry::Types::PredicateInferrer.new.[](tree)).to eql([[[:int?], [:array?]]])
  end
end
