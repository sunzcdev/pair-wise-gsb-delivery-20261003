# frozen_string_literal: true

RSpec.describe Shoryuken::Helpers::FetchCancellation do
  subject(:cancellation) { described_class.new }

  it 'starts not cancelled' do
    expect(cancellation.cancelled?).to be(false)
  end

  describe '#cancel' do
    it 'flips the flag' do
      expect(cancellation.cancel).to be(true)
      expect(cancellation.cancelled?).to be(true)
    end

    it 'is idempotent and reports only the first cancellation' do
      expect(cancellation.cancel).to be(true)
      expect(cancellation.cancel).to be(false)
      expect(cancellation.cancelled?).to be(true)
    end
  end

  it 'publishes the flag across threads' do
    thread = Thread.new do
      sleep 0.01 until cancellation.cancelled?
      :observed
    end

    cancellation.cancel

    expect(thread.join(5).value).to eq(:observed)
  end
end
