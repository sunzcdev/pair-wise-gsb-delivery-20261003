module Interactor
  # Public: A cancellation signal shared between a caller and an
  # Interactor::Organizer. The caller may cancel the signal at any time,
  # optionally providing a reason. The organizer checks the signal at safe
  # boundaries between steps and, once the signal is cancelled, stops invoking
  # further steps and rolls back the steps that already completed. A step that
  # is currently running is never interrupted.
  #
  # Pass the signal to the organizer through the context:
  #
  # Examples
  #
  #   signal = Interactor::Cancellation.new
  #   # => #<Interactor::Cancellation>
  #   signal.cancelled?
  #   # => false
  #   signal.cancel!("client went away")
  #   # => #<Interactor::Cancellation>
  #   signal.cancelled?
  #   # => true
  #   signal.reason
  #   # => "client went away"
  #
  #   MyOrganizer.call(cancellation: signal)
  class Cancellation
    # Public: Gets the reason given when the signal was cancelled, or nil if
    # no reason was provided.
    attr_reader :reason

    # Public: Whether the cancellation signal has been cancelled.
    #
    # Examples
    #
    #   signal = Interactor::Cancellation.new
    #   # => #<Interactor::Cancellation>
    #   signal.cancelled?
    #   # => false
    #   signal.cancel!
    #   signal.cancelled?
    #   # => true
    #
    # Returns true if cancelled or false otherwise.
    def cancelled?
      @cancelled || false
    end

    # Public: Cancel the signal, optionally providing a reason. Cancellation
    # is permanent; create a new signal for each invocation.
    #
    # reason - An optional object describing why the caller cancelled.
    #          (default: nil)
    #
    # Examples
    #
    #   signal = Interactor::Cancellation.new
    #   # => #<Interactor::Cancellation>
    #   signal.cancel!("client went away")
    #   # => #<Interactor::Cancellation>
    #   signal.reason
    #   # => "client went away"
    #
    # Returns the Interactor::Cancellation signal.
    def cancel!(reason = nil)
      @cancelled = true
      @reason = reason
      self
    end
  end
end
