# frozen_string_literal: true

module StateMachines
  # The observable result of replaying an object's deferred events after a
  # successful transition.  Each replayed entry ends up in exactly one of the
  # following buckets:
  # * +fired+ - entries whose event transitioned successfully
  # * +redeferred+ - entries whose event still could not transition and were
  #   placed back onto the object's deferred queue
  # * +failed+ - <tt>[entry, error]</tt> pairs for entries whose replay raised
  #   an error (or whose event could no longer be fired or deferred)
  class DeferredReplayReport
    # The deferred entries that transitioned successfully during replay
    attr_reader :fired

    # The deferred entries that were deferred again during replay
    attr_reader :redeferred

    # The deferred entries that failed to replay, as [entry, error] pairs
    attr_reader :failed

    def initialize
      @fired = []
      @redeferred = []
      @failed = []
    end

    # The total number of entries processed during this replay round
    def processed
      fired.length + redeferred.length + failed.length
    end

    def inspect
      "#<#{self.class} fired=#{fired.length} redeferred=#{redeferred.length} failed=#{failed.length}>"
    end
  end
end
