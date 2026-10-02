require 'spec_helper'

describe 'explicit target state arguments' do
  let(:example) { ExplicitToStateExample.new }

  it 'keeps a symbol with the same name as a reachable target as a business argument' do
    example.review(:pending)

    expect(example).to be_pending
    expect(example.guard_arguments).to eq([[:pending]])
    expect(example.callback_arguments).to eq([[:pending]])
  end

  it 'selects the target with the separate to_state option' do
    example.review(:approved, to_state: :rejected)

    expect(example).to be_rejected
    expect(example.callback_arguments).to eq([[:approved]])
  end

  it 'keeps a first positional hash as a business argument' do
    example.review({ approved: true })

    expect(example).to be_pending
    expect(example.guard_arguments).to eq([[{ approved: true }]])
    expect(example.callback_arguments).to eq([[{ approved: true }]])
  end

  it 'keeps business keyword arguments and selects the target' do
    example.review(reason: 'policy', to_state: :rejected)

    expect(example).to be_rejected
    expect(example.guard_arguments).to eq([[{ reason: 'policy' }]])
    expect(example.callback_arguments).to eq([[{ reason: 'policy' }]])
  end

  it 'also accepts the shorter to target option' do
    example.review(:approved, to: :rejected)

    expect(example).to be_rejected
    expect(example.callback_arguments).to eq([[:approved]])
  end

  it 'uses the first candidate when no target is specified' do
    example.review(:note)

    expect(example).to be_pending
  end

  it 'fails before guards and callbacks for an unreachable target' do
    expect { example.review(:note, to_state: :approved) }
      .to raise_error(AASM::InvalidTransition)

    expect(example).to be_approved
    expect(example.guard_arguments).to be_empty
    expect(example.callback_arguments).to be_empty
  end

  it 'reports target availability through may_fire_event?' do
    expect(example.aasm.may_fire_event?(:review, :pending)).to be(true)
    expect(example.aasm.may_fire_event?(:review, :note, to_state: :rejected)).to be(true)
    expect(example.aasm.may_fire_event?(:review, to_state: :approved)).to be(false)
  end

  it 'reports target availability through generated predicate methods' do
    expect(example.may_review?(:pending)).to be(true)
    expect(example.may_review?(:note, to_state: :rejected)).to be(true)
    expect(example.may_review?(to_state: :approved)).to be(false)
  end

  it 'supports dynamic fire and fire!' do
    expect(example.aasm.fire(:review, :note, to_state: :rejected)).to be(true)
    expect(example).to be_rejected

    pending = ExplicitToStateExample.new
    pending.review(to_state: :pending)
    expect(pending.aasm.fire!(:reopen, to_state: :approved)).to be(true)
    expect(pending).to be_approved
  end

  it 'supports generated bang methods' do
    rejected = example.review!(to_state: :rejected)

    expect(rejected).to be(true)
    expect(example).to be_rejected
  end

  it 'applies the same rules to named state machines' do
    named = ExplicitToStateMultipleExample.new

    expect(named.may_review?(:pending, to_state: :rejected)).to be(true)
    named.review(:note, to_state: :rejected)
    expect(named.aasm(:review).current_state).to eq(:rejected)

    named = ExplicitToStateMultipleExample.new
    expect(named.aasm(:review).may_fire_event?(:review, :pending)).to be(true)
    expect(named.aasm(:review).fire(:review, :note, to_state: :rejected)).to be(true)
    expect(named.aasm(:review).current_state).to eq(:rejected)
  end

  it 'preserves legacy target selection when explicit mode is disabled' do
    legacy = ParametrisedEvent.new

    legacy.wakeup(:working)
    expect(legacy).to be_working
  end
end
