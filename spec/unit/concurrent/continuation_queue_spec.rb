# frozen_string_literal: true

require "spec_helper"

describe Bunny::Concurrent::ContinuationQueue do
  it "atomically drains all pending items" do
    queue = described_class.new
    queue.push(:first)
    queue.push(:second)

    expect(queue.drain).to eq([:first, :second])
    expect(queue).to be_empty
  end

  it "raises Bunny::ClientTimeout when the timeout expires" do
    expect { described_class.new.poll(20) }.to raise_error(Bunny::ClientTimeout)
  end
end
