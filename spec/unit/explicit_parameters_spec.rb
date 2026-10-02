require 'spec_helper'

class ExplicitParameterReview
  include AASM

  aasm explicit_parameters: true do
    state :pending, initial: true
    state :approved
    state :rejected

    event :review do
      before :before_review
      after :after_review

      transitions from: :pending, to: :approved, guard: :allow_review?
      transitions from: :pending, to: :rejected, guard: :reject?
    end
  end

  attr_reader :guard_calls, :before_calls, :after_calls
  attr_accessor :reject_review

  def initialize
    @guard_calls = []
    @before_calls = []
    @after_calls = []
    @reject_review = false
  end

  def allow_review?(*args, **kwargs)
    @guard_calls << [args, kwargs]
    true
  end

  def reject?(*args, **kwargs)
    @guard_calls << [args, kwargs]
    reject_review
  end

  def before_review(*args, **kwargs)
    @before_calls << [args, kwargs]
  end

  def after_review(*args, **kwargs)
    @after_calls << [args, kwargs]
  end
end

class NamedExplicitParameterReview
  include AASM

  aasm(:review, explicit_parameters: true, namespace: :status) do
    state :pending, initial: true
    state :approved

    event :review do
      after :after_review
      transitions from: :pending, to: :approved
    end
  end

  attr_reader :after_calls

  def initialize
    @after_calls = []
  end

  def after_review(*args, **kwargs)
    @after_calls << [args, kwargs]
  end
end

describe 'explicit event parameters' do
  let(:review) { ExplicitParameterReview.new }

  it 'keeps the first positional symbol when the destination has the same name' do
    expect(review.review(:approved, to: :approved)).to be(true)

    expect(review.aasm.current_state).to eq(:approved)
    expect(review.before_calls).to eq([[[:approved], {}]])
    expect(review.after_calls).to eq([[[:approved], {}]])
  end

  it 'passes a positional hash to guards and callbacks' do
    payload = { decision: :approved }

    expect(review.review(payload, to: :approved)).to be(true)

    expect(review.aasm.current_state).to eq(:approved)
    expect(review.guard_calls).to eq([[[payload], {}]])
    expect(review.before_calls).to eq([[[payload], {}]])
  end

  it 'separates keyword arguments from the destination option' do
    expect(review.review(approved: true, to: :approved)).to be(true)

    expect(review.aasm.current_state).to eq(:approved)
    expect(review.before_calls).to eq([[[], { approved: true }]])
    expect(review.after_calls).to eq([[[], { approved: true }]])
  end

  it 'uses the first allowed transition when no destination is given' do
    review.reject_review = true

    expect(review.review(approved: false)).to be(true)

    expect(review.aasm.current_state).to eq(:approved)
  end

  it 'selects another destination for an event with multiple targets' do
    review.reject_review = true

    expect(review.review(:business_argument, to: :rejected)).to be(true)

    expect(review.aasm.current_state).to eq(:rejected)
    expect(review.before_calls).to eq([[[:business_argument], {}]])
    expect(review.after_calls).to eq([[[:business_argument], {}]])
  end

  it 'checks may_fire_event? with an explicit destination and business arguments' do
    expect(review.aasm.may_fire_event?(:review, :approved, to: :approved)).to be(true)
    expect(review.guard_calls).to eq([[[:approved], {}]])

    review.reject_review = true
    expect(review.aasm.may_fire_event?(:review, to: :rejected)).to be(true)
  end

  it 'returns false from may_fire_event? for an unreachable destination before callbacks run' do
    expect(review.aasm.may_fire_event?(:review, to: :missing)).to be(false)

    expect(review.aasm.current_state).to eq(:pending)
    expect(review.guard_calls).to be_empty
    expect(review.before_calls).to be_empty
  end

  it 'works through dynamic fire' do
    review.reject_review = true

    expect(review.aasm.fire(:review, :business_argument, to: :rejected)).to be(true)

    expect(review.aasm.current_event).to eq(:review)
    expect(review.aasm.current_state).to eq(:rejected)
    expect(review.before_calls).to eq([[[:business_argument], {}]])
  end

  it 'works through dynamic fire!' do
    review.reject_review = true

    expect(review.aasm.fire!(:review, :business_argument, to: :rejected)).to be(true)

    expect(review.aasm.current_event).to eq(:review!)
    expect(review.aasm.current_state).to eq(:rejected)
    expect(review.after_calls).to eq([[[:business_argument], {}]])
  end

  it 'fails dynamic fire before callbacks when the destination is unreachable' do
    expect do
      review.aasm.fire(:review, to: :missing)
    end.to raise_error(AASM::InvalidTransition)

    expect(review.aasm.current_state).to eq(:pending)
    expect(review.before_calls).to be_empty
  end

  it 'fails before callbacks when the destination is unreachable' do
    expect do
      review.review(to: :missing)
    end.to raise_error(AASM::InvalidTransition)

    expect(review.aasm.current_state).to eq(:pending)
    expect(review.guard_calls).to be_empty
    expect(review.before_calls).to be_empty
    expect(review.after_calls).to be_empty
  end

  it 'does not wrap a single business parameter in an array' do
    review.review(:approved, to: :approved)

    expect(review.after_calls.first.first).to eq([:approved])
  end

  context 'with a named state machine' do
    let(:review) { NamedExplicitParameterReview.new }

    it 'uses the same rules through generated and dynamic methods' do
      expect(review.aasm(:review).may_fire_event?(:review, to: :approved)).to be(true)
      expect(review.aasm(:review).fire(:review, approved: true, to: :approved)).to be(true)

      expect(review.aasm(:review).current_state).to eq(:approved)
      expect(review.after_calls).to eq([[[], { approved: true }]])

      second_review = NamedExplicitParameterReview.new
      second_review.review_status(:approved, to: :approved)

      expect(second_review.aasm(:review).current_state).to eq(:approved)
      expect(second_review.after_calls).to eq([[[:approved], {}]])
    end
  end
end
