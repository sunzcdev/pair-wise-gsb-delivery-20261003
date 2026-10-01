# frozen_string_literal: true

require 'blueprinter/view_collection'

describe Blueprinter::RenderSnapshot do
  let(:view_collection) { Blueprinter::ViewCollection.new }
  let(:other_view_collection) { Blueprinter::ViewCollection.new }

  describe '.current' do
    it 'is nil outside of a render' do
      expect(described_class.current).to be_nil
    end
  end

  describe '.wrap' do
    it 'activates the snapshot for the duration of the block and restores nil afterwards' do
      seen = nil
      described_class.wrap { |snapshot| seen = described_class.current.equal?(snapshot) }

      expect(seen).to be(true)
      expect(described_class.current).to be_nil
    end

    it 'restores the previously active snapshot when nested, even if the inner block raises' do
      outer_snapshot = nil
      restored_current = nil

      described_class.wrap do |snapshot|
        outer_snapshot = snapshot
        begin
          described_class.wrap { |_inner_snapshot| raise 'boom' }
        rescue RuntimeError
          # expected: the inner render raised
        end
        restored_current = described_class.current
      end

      expect(restored_current).to be(outer_snapshot)
      expect(described_class.current).to be_nil
    end

    it 'restores the previous snapshot when the block raises' do
      expect do
        described_class.wrap { raise 'boom' }
      end.to raise_error('boom')

      expect(described_class.current).to be_nil
    end
  end

  describe '#pin' do
    it 'returns the same compiled structure on repeated encounters' do
      described_class.wrap do |snapshot|
        first = snapshot.pin(view_collection)
        view_collection[:new_view]
        second = snapshot.pin(view_collection)

        expect(second).to equal(first)
      end
    end

    it 'keeps independent pins for different view collections' do
      described_class.wrap do |snapshot|
        first = snapshot.pin(view_collection)
        second = snapshot.pin(other_view_collection)

        expect(first).not_to equal(second)
        expect(snapshot.pin(other_view_collection)).to equal(second)
        expect(snapshot.pin(view_collection)).to equal(first)
      end
    end

    it 'does not freeze the view collection itself' do
      described_class.wrap do |snapshot|
        snapshot.pin(view_collection)

        expect(view_collection).not_to be_frozen
      end
    end
  end
end
