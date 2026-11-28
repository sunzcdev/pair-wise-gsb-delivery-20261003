require "flipper/adapters/wrapper"

module Flipper
  module Adapters
    class ActorLimit < Wrapper
      LimitExceeded = Class.new(Flipper::Error)

      attr_reader :limit

      class << self
        # Returns whether sync mode is enabled for the current thread.
        # When sync mode is enabled, actor limits are not enforced,
        # allowing sync operations to bring local state in line with
        # remote state regardless of limits.
        def sync_mode
          Thread.current[:flipper_actor_limit_sync_mode]
        end

        def sync_mode=(value)
          Thread.current[:flipper_actor_limit_sync_mode] = value
        end

        # Executes a block with sync mode enabled. Actor limits will
        # not be enforced within the block.
        def with_sync_mode
          old_value = sync_mode
          self.sync_mode = true
          yield
        ensure
          self.sync_mode = old_value
        end
      end

      def initialize(adapter, limit = 100)
        super(adapter)
        @limit = limit
        # Serializes the plan-and-write of actor replacements for adapters that
        # only expose per-member writes. The wrapped adapter's own atomic
        # #replace re-checks the members under its lock, which is the
        # authoritative decision when another process changes them.
        @replace_lock = Mutex.new
      end

      def enable(feature, gate, resource)
        if gate.is_a?(Flipper::Gates::Actor) && !self.class.sync_mode && over_limit?(feature, resource)
          raise LimitExceeded, "Actor limit of #{@limit} exceeded for feature #{feature.key}. See https://www.flippercloud.io/docs/features/actors#limitations"
        else
          super
        end
      end

      # Public: Replaces one actor with another as a single, capacity-bounded
      # set change.
      #
      # A replacement never grows the set: the old member leaves as part of the
      # same change, so a set that is already at the limit can still swap a
      # member and the total stays within the limit for the whole change. A
      # change that is not actually a replacement (neither member is enabled,
      # so applying it would only add one) raises Flipper::ActorSetConflict
      # rather than growing the set.
      #
      # The wrapped adapter does the write. Stores that can apply the pair
      # atomically do so, so readers never see the old member removed before
      # the new one exists; stores that only expose per-member writes use the
      # add-then-remove fallback, which leaves the original member enabled when
      # the new one cannot be added.
      #
      # expected - optional member list the caller observed. When it no longer
      #            matches, nothing is written and Flipper::ActorSetConflict is
      #            raised, which is what makes two concurrent replacements
      #            deterministic: one applies, the other is told to retry.
      #
      # Returns true.
      # Raises Flipper::ActorSetConflict when the members are stale.
      # Raises LimitExceeded when the change would grow past the limit.
      def replace(feature, gate, old_thing, new_thing, expected: nil)
        unless gate.is_a?(Flipper::Gates::Actor)
          return adapter.replace(feature, gate, old_thing, new_thing, expected: expected)
        end

        if self.class.sync_mode
          return adapter.replace(feature, gate, old_thing, new_thing, expected: expected)
        end

        @replace_lock.synchronize do
          current = normalize_member_values(adapter.get(feature)[gate.key])
          prospective = plan_member_replacement(
            current,
            old_value: old_thing.value,
            new_value: new_thing.value,
            expected: expected,
            feature_key: feature.key,
          )

          if prospective.size > @limit && prospective.size > current.size
            raise LimitExceeded, "Actor limit of #{@limit} exceeded for feature #{feature.key}. See https://www.flippercloud.io/docs/features/actors#limitations"
          end

          return true if prospective == current

          adapter.replace(feature, gate, old_thing, new_thing, expected: expected)
          true
        end
      end

      private

      def over_limit?(feature, resource)
        actors = feature.actors_value
        actors.size >= @limit && !actors.include?(resource.value)
      end
    end
  end
end
