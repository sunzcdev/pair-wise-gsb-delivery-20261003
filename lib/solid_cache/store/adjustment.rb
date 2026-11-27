# frozen_string_literal: true

module SolidCache
  class Store
    # Namespace for the values returned by SolidCache::Store#adjust_multi.
    #
    # A batch of counter adjustments can span several shards, and a shard can be
    # temporarily unreachable. Every counter is therefore reported on its own: the
    # ones that reached their shard carry the new value, the ones on a failed shard
    # carry the shard and the error that stopped them.
    #
    # A batch can also be identified with a +token+. The token is recorded in the
    # same transaction that changes each counter, so resending the same batch with
    # the same token never applies an adjustment twice - the affected counters are
    # reported as ALREADY_APPLIED instead.
    module Adjustment
      APPLIED = :applied
      ALREADY_APPLIED = :already_applied
      UNAVAILABLE = :unavailable

      # One requested counter change in a batch.
      Change = Struct.new(:name, :key, :amount, keyword_init: true)

      # What happened to one requested counter change.
      class Outcome
        attr_reader :name, :shard, :status, :value, :error

        def initialize(name:, shard:, status:, value: nil, error: nil)
          @name = name
          @shard = shard
          @status = status
          @value = value
          @error = error
        end

        def applied?
          status == APPLIED || status == ALREADY_APPLIED
        end

        def already_applied?
          status == ALREADY_APPLIED
        end

        def unavailable?
          status == UNAVAILABLE
        end

        def to_h
          { shard: shard, status: status, value: value, error: error }
        end

        def inspect
          "#<#{self.class.name} name=#{name.inspect} shard=#{shard.inspect} status=#{status.inspect} value=#{value.inspect}>"
        end
      end

      # The result of one #adjust_multi call. Iterates the outcomes in the order the
      # counters were requested.
      class Result
        include Enumerable

        attr_reader :store, :token, :changes

        def initialize(store:, token:, changes:, options:, outcomes:)
          @store = store
          @token = token
          @changes = changes
          @options = options
          @outcomes = outcomes
        end

        def each
          return to_enum(:each) unless block_given?

          changes.each { |change| yield outcome_for(change) }
        end

        def [](name)
          change = changes.find { |candidate| candidate.name == name || candidate.key == name }
          outcome_for(change) if change
        end

        def keys
          changes.map(&:name)
        end

        def empty?
          changes.empty?
        end

        # Every adjustment in the batch landed (or was already recorded by a resend).
        def complete?
          changes.all? { |change| outcome_for(change)&.applied? }
        end

        # Name => new value for the adjustments that landed.
        def values
          changes.each_with_object({}) do |change, results|
            outcome = outcome_for(change)
            results[change.name] = outcome.value if outcome&.applied?
          end
        end

        # Names of the adjustments this call could not make.
        def unavailable
          names_for(UNAVAILABLE)
        end

        # Names this call left alone because an earlier attempt of the same batch
        # had already recorded them.
        def already_applied
          names_for(ALREADY_APPLIED)
        end

        def unavailable_shards
          changes.filter_map { |change| outcome_for(change).shard if outcome_for(change)&.unavailable? }.uniq
        end

        # Resends only the adjustments this result could not make, reusing the batch
        # token. Counters that did land are not touched a second time.
        def retry(**options)
          pending = changes.reject { |change| outcome_for(change)&.applied? }
          return self if pending.empty?

          store.adjust_multi(pending.to_h { |change| [ change.name, change.amount ] }, token: token, **@options, **options)
        end

        def to_h
          changes.each_with_object({}) do |change, results|
            results[change.name] = outcome_for(change)&.to_h
          end
        end

        def inspect
          counts = group_by(&:status).transform_values(&:size)
          "#<#{self.class.name} token=#{token.inspect} counts=#{counts.inspect}>"
        end

        private
          def outcome_for(change)
            @outcomes[change.key]
          end

          def names_for(status)
            changes.filter_map { |change| change.name if outcome_for(change)&.status == status }
          end
      end
    end
  end
end