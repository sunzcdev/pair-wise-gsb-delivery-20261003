# frozen_string_literal: true

require_relative 'model_base'

# An order fulfillment flow with hierarchical states.  The "processing" and
# "done" states group shared behavior for their child states:
#
#   pending
#   processing
#     validating
#     saving
#   done
#     archived
#   cancelled
class Order < ModelBase
  attr_accessor :cancellation_allowed
  attr_reader :log

  def initialize(attributes = {})
    @log = []
    @cancellation_allowed = false
    attributes.each { |attr, value| send("#{attr}=", value) }
    super()
  end

  state_machine initial: :pending do
    state :processing
    state :validating, parent: :processing
    state :saving, parent: :processing
    state :done
    state :archived, parent: :done

    on_enter(:processing) { |order| order.log << :enter_processing }
    on_exit(:processing)  { |order| order.log << :exit_processing }
    on_enter(:validating) { |order| order.log << :enter_validating }
    on_exit(:validating)  { |order| order.log << :exit_validating }
    on_enter(:saving)     { |order| order.log << :enter_saving }
    on_exit(:saving)      { |order| order.log << :exit_saving }
    on_enter(:done)       { |order| order.log << :enter_done }
    on_enter(:archived)   { |order| order.log << :enter_archived }
    on_enter(:cancelled)  { |order| order.log << :enter_cancelled }

    after_failure(on: :cancel) { |order| order.log << :cancel_failed }

    event :start do
      transition pending: :validating
    end

    event :advance do
      transition validating: :saving
    end

    event :redo_validation do
      transition saving: :validating
    end

    # Defined on the parent state: fires from any child of :processing
    event :finish do
      transition processing: :archived
    end

    # Parent-level interception: only fires when the guard passes
    event :cancel do
      transition processing: :cancelled, if: :cancellation_allowed
    end
  end
end
