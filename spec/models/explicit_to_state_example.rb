class ExplicitToStateExample
  include AASM

  attr_reader :guard_arguments, :callback_arguments

  aasm explicit_to_state: true do
    state :approved, initial: true
    state :rejected
    state :pending

    event :review do
      transitions from: :approved, to: :pending, guard: :allow_review?, after: :record_review
      transitions from: :approved, to: :rejected, guard: :allow_review?, after: :record_review
    end

    event :reopen do
      transitions from: :pending, to: :approved, after: :record_review
      transitions from: :rejected, to: :approved, after: :record_review
    end
  end

  def initialize
    @guard_arguments = []
    @callback_arguments = []
  end

  def allow_review?(*arguments)
    @guard_arguments << arguments
    true
  end

  def record_review(*arguments)
    @callback_arguments << arguments
  end
end

class ExplicitToStateMultipleExample
  include AASM

  aasm(:review, explicit_to_state: true) do
    state :approved, initial: true
    state :rejected
    state :pending

    event :review do
      transitions from: :approved, to: :pending
      transitions from: :approved, to: :rejected
    end
  end
end
