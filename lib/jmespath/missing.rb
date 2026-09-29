# frozen_string_literal: true
module JMESPath
  # @api private
  #
  # Internal sentinel used only by the diagnostic mode to distinguish a
  # missing key/index from an explicit `null` value. It is never returned
  # to callers: {Runtime#diagnose} converts it to `nil` (plus a diagnostic
  # entry) before building its report, and the default {Runtime#search}
  # never produces it.
  MISSING = Object.new

  class << MISSING
    def inspect
      '#<JMESPath::MISSING>'
    end
    alias to_s inspect
  end

  MISSING.freeze
end
