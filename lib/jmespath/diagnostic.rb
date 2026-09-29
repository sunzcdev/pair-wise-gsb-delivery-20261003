# frozen_string_literal: true
module JMESPath
  # Diagnostic evaluation mode.
  #
  # In diagnostic mode, accessing a key that is absent from the data
  # produces an internal {Missing} sentinel instead of `nil`. The
  # sentinel propagates through the expression and is replaced by a
  # serializable Hash before the result is returned, so the Ruby
  # sentinel object never leaks into public results.
  #
  # @api private
  module Diagnostic
    THREAD_KEY = :jmespath_diagnostic_mode

    def self.active?
      Thread.current[THREAD_KEY] ? true : false
    end

    def self.activate
      previous = Thread.current[THREAD_KEY]
      Thread.current[THREAD_KEY] = true
      yield
    ensure
      Thread.current[THREAD_KEY] = previous
    end

    # Recursively replaces {Missing} sentinels with serializable Hashes.
    def self.sanitize(value)
      case value
      when Missing
        value.to_h
      when Array
        value.map { |element| sanitize(element) }
      when Hash
        value.each_with_object({}) do |(key, element), hash|
          hash[key] = sanitize(element)
        end
      else
        value
      end
    end
  end
end
