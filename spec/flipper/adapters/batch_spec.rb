require 'flipper/adapters/batch'

RSpec.describe Flipper::Adapters::Batch do
  let(:source_adapter) { Flipper::Adapters::Memory.new }
  let(:flipper) { Flipper.new(source_adapter) }

  subject { described_class.from(source_adapter) }

  it_should_behave_like 'a flipper adapter'

  it 'starts empty when built without a source' do
    batch = described_class.new
    expect(batch.features).to eq(Set.new)
    expect(batch.get_all).to eq({})
    expect(batch.changes).to be_empty
  end

  it 'seeds itself with the committed state of the source adapter' do
    flipper.enable(:stats)
    flipper.add(:search)
    flipper.enable_actor(:search, Flipper::Actor.new('22'))

    expect(subject.get(flipper[:stats])[:boolean]).to eq('true')
    expect(subject.get(flipper[:search])[:actors]).to eq(Set['22'])
    expect(subject.features).to eq(Set['stats', 'search'])
  end

  it 'does not mutate the source adapter when written to' do
    flipper.enable(:stats)

    subject.add(flipper[:search])
    subject.enable(flipper[:search], flipper[:search].gate(:boolean), Flipper::Types::Boolean.new(true))

    expect(source_adapter.features).to eq(Set['stats'])
    expect(source_adapter.get(flipper[:search])).to eq(source_adapter.default_config)
    expect(subject.features).to eq(Set['stats', 'search'])
  end

  it 'tracks which features were changed' do
    feature = flipper[:stats]
    expect(subject.changed?(feature)).to be(false)

    subject.add(feature)
    expect(subject.changed?(feature)).to be(true)
    expect(subject.changes).to eq(Set['stats'])
  end

  it 'returns an importable snapshot via get_all' do
    flipper.add(:stats)
    flipper.enable_actor(:stats, Flipper::Actor.new('22'))
    feature = flipper[:stats]
    # A boolean enable resets the other gates (matching Memory), so re-add
    # the actor afterwards to keep both values in the snapshot.
    subject.enable(feature, feature.gate(:boolean), Flipper::Types::Boolean.new(true))
    subject.enable(feature, feature.gate(:actor), Flipper::Types::Actor.new(Flipper::Actor.new('22')))

    destination = Flipper::Adapters::Memory.new
    destination.import(subject)

    expect(destination.get(feature)[:boolean]).to eq('true')
    expect(destination.get(feature)[:actors]).to eq(Set['22'])
  end

  it 'duplicates gate sets so committing shares no mutable state with the batch' do
    flipper.add(:stats)
    flipper.enable_actor(:stats, Flipper::Actor.new('22'))

    destination = Flipper::Adapters::Memory.new
    destination.import(subject)

    subject.get(flipper[:stats])[:actors] << '99'

    expect(destination.get(flipper[:stats])[:actors]).to eq(Set['22'])
  end
end
