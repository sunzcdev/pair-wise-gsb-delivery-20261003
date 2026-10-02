module Interactor
  # Public: A cooperative signal shared by an organizer and its nested
  # interactors. The signal is checked only at organizer boundaries, so running
  # user methods are never interrupted.
  class Cancellation
    attr_reader :reason

    # Public: Build and cancel a signal in one step.
    #
    # reason - An optional object explaining why work was cancelled.
    #
    # Returns a cancelled Cancellation.
    def self.cancel(reason = nil)
      new.tap { |cancellation| cancellation.cancel(reason) }
    end

    # Public: Initialize a Cancellation.
    #
    # reason - An optional initial cancellation reason.
    def initialize(reason = nil)
      @reason = reason
    end

    # Public: Mark the signal as cancelled.
    #
    # reason - An optional explanation for the cancellation. When omitted, the
    #          previous reason is preserved.
    #
    # Returns true.
    def cancel(reason = nil)
      @reason = reason unless reason.nil?
      @cancelled = true
    end

    # Public: Whether cancellation has been requested.
    def cancelled?
      @cancelled || false
    end

    # Public: Alias for American spelling.
    alias_method :canceled?, :cancelled?

    # Public: Bang-style alias for #cancel.
    alias_method :cancel!, :cancel
  end
end
