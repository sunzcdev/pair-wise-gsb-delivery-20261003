# frozen_string_literal: true

module AASM
  # Result of a single availability inspection (see AASM::InstanceBase#availability).
  #
  # The result is a snapshot for display purposes only. It does not authorize
  # any later event call and must not be cached: guards must be re-evaluated
  # whenever the inspected object changes.
  class Availability
    # [Array<AASM::Core::Event>] events with at least one allowed candidate
    # transition from the current state
    attr_reader :events

    # [Array<Hash>] allowed candidate transitions as
    #   { event:, state:, transition: }
    attr_reader :transitions

    # [Array<Hash>] one entry per rejected candidate transition
    attr_reader :rejections

    # [Array<Hash>] full evaluation report for every candidate transition
    attr_reader :candidates

    def initialize(events:, transitions:, rejections:, candidates:)
      @events = events
      @transitions = transitions
      @rejections = rejections
      @candidates = candidates
    end

    def permitted_events
      events.map(&:name)
    end

    def permitted_transitions
      transitions
    end

    # Rejection entries for a single event name, without information collected
    # while inspecting any other event or object.
    def rejections_for(event_name)
      rejections.select { |rejection| rejection[:event] == event_name }
    end
  end
end
