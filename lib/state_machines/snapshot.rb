# frozen_string_literal: true

module StateMachines
  # Helpers for safely exporting and restoring state machine snapshots.
  #
  # Snapshots are plain, JSON-compatible data structures (hashes, arrays,
  # strings, numbers, booleans and nil only).  They can be persisted with
  # JSON.generate / JSON.parse and are never used to (de)serialize arbitrary
  # Ruby objects.
  module Snapshot
    module_function

    # Determines whether the given value can be faithfully represented in
    # JSON: nil, true, false, strings, integers, finite floats, arrays of
    # JSON-compatible values, and hashes with string keys and JSON-compatible
    # values.
    def json_compatible?(value)
      case value
      when nil, true, false, String, Integer
        true
      when Float
        value.finite?
      when Array
        value.all? { |element| json_compatible?(element) }
      when Hash
        value.all? { |key, element| key.is_a?(String) && json_compatible?(element) }
      else
        false
      end
    end
  end
end
