require 'set'
require 'flipper/errors'

module Flipper
  # Adding a module include so we have some hooks for stuff down the road
  module Adapter
    def self.included(base)
      base.extend(ClassMethods)
    end

    module ClassMethods
      # Public: Default config for a feature's gate values.
      def default_config
        {
          boolean: nil,
          groups: Set.new,
          actors: Set.new,
          expression: nil,
          percentage_of_actors: nil,
          percentage_of_time: nil,
        }
      end

      def from(source)
        return source if source.is_a?(Flipper::Adapter)
        source.adapter
      end
    end

    def read_only?
      false
    end

    # Public: Get all features and gate values in one call. Defaults to one call
    # to features and another to get_multi. Feel free to override per adapter to
    # make this more efficient.
    def get_all(**kwargs)
      instances = features.map { |key| Flipper::Feature.new(key, self) }
      get_multi(instances)
    end

    # Public: Get multiple features in one call. Defaults to one get per
    # feature. Feel free to override per adapter to make this more efficient and
    # reduce network calls.
    def get_multi(features)
      result = {}
      features.each do |feature|
        result[feature.key] = get(feature)
      end
      result
    end

    # Public: Ensure that adapter is in sync with source adapter provided.
    #
    # source - The source dsl, adapter or export to import.
    #
    # Returns true if successful.
    def import(source)
      Adapters::Sync::Synchronizer.new(self, self.class.from(source), raise: true).call
      true
    end

    # Public: Exports the adapter in a given format for a given format version.
    #
    # Returns a Flipper::Export instance.
    def export(format: :json, version: 1)
      Flipper::Exporter.build(format: format, version: version).call(self)
    end

    # Public: Default config for a feature's gate values.
    def default_config
      self.class.default_config
    end

    # Public: Replaces one member of a set-type gate with another as a single
    # logical set change, instead of a disable followed by an enable.
    #
    # The decision is planned against the members the adapter reports before
    # anything is written, so:
    #
    # * a set that is already at its limit can still swap a member, because
    #   the old member leaves as part of the same change and the size never
    #   grows;
    # * a change that would not actually be a replacement (neither the old nor
    #   the new member is present) raises Flipper::ActorSetConflict instead of
    #   silently adding a member, so the set cannot grow past its limit;
    # * when `expected` is supplied and no longer matches the members that
    #   exist now, nothing is written and Flipper::ActorSetConflict is raised.
    #   That is what gives two concurrent replacements a deterministic result:
    #   the one whose observed members still match applies the change and the
    #   other is told to re-read and retry.
    #
    # Adapters that can write the pair atomically (Memory, PStore, Redis,
    # ActiveRecord, Sequel, Mongo) override this so no reader can observe the
    # set without the old member and before the new one exists. This default is
    # for adapters that only expose per-member writes. It adds the new member
    # before removing the old one, so if the new member cannot be added,
    # nothing was removed and the original member stays enabled.
    #
    # feature   - The Flipper::Feature being changed.
    # gate      - The :set gate being changed (Flipper::Gates::Actor, etc).
    # old_thing - The wrapped thing that should no longer be enabled.
    # new_thing - The wrapped thing that should be enabled in its place.
    # expected  - Optional Enumerable of the member values the caller observed
    #             as the current members. A mismatch raises
    #             Flipper::ActorSetConflict before anything is written.
    #
    # Returns true when the replacement was applied (or was already the case).
    # Raises Flipper::ActorSetConflict when the members are not what the caller
    # observed, or when neither the old nor the new member is enabled.
    def replace(feature, gate, old_thing, new_thing, expected: nil)
      current = normalize_member_values(get(feature)[gate.key])
      prospective = plan_member_replacement(
        current,
        old_value: old_thing.value,
        new_value: new_thing.value,
        expected: expected,
        feature_key: feature.key,
      )

      return true if prospective == current

      new_was_enabled = current.include?(new_thing.value.to_s)

      # Add before remove: a failed add leaves the original member enabled.
      enable(feature, gate, new_thing)
      begin
        disable(feature, gate, old_thing)
      rescue StandardError
        # Best effort to undo the add so a failed removal does not leave the
        # set larger than it was. The original failure is the one to surface.
        unless new_was_enabled
          begin
            disable(feature, gate, new_thing)
          rescue StandardError
            nil
          end
        end
        raise
      end

      true
    end

    # Public: Replaces one actor with another in the actor gate. Convenience
    # for Feature#replace_actor / DSL#replace_actor when calling an adapter
    # directly.
    #
    # Returns true, or raises like #replace.
    def replace_actor(feature, old_actor, new_actor, expected: nil)
      replace(
        feature,
        feature.gate(:actor),
        Flipper::Types::Actor.wrap(old_actor),
        Flipper::Types::Actor.wrap(new_actor),
        expected: expected,
      )
    end

    # Public: The Flipper::SyncVersion of the synced flags this adapter holds,
    # or nil if it doesn't know. Syncs use it to never replace newer flags with
    # older ones. Adapters that wrap another adapter delegate to it. Storage
    # adapters that can keep it override this and #sync_version=.
    def sync_version
      wrapped = respond_to?(:adapter) ? adapter : nil
      wrapped.sync_version if wrapped.respond_to?(:sync_version)
    end

    # Public: Records the Flipper::SyncVersion of the flags just synced into
    # this adapter. Does nothing for adapters that can't keep it.
    def sync_version=(version)
      wrapped = respond_to?(:adapter) ? adapter : nil
      wrapped.sync_version = version if wrapped.respond_to?(:sync_version=)
    end

    # Public: default name of the adapter
    def name
      @name ||= self.class.name.split('::').last.split(/(?=[A-Z])/).join('_').downcase.to_sym
    end

    # Public: Returns a string representation of the adapter stack for debugging.
    # Shows the full chain of wrapped adapters.
    #
    # Examples:
    #   "memoizable -> active_support_cache_store -> active_record"
    #   "memoizable -> failover(primary: redis, secondary: memory)"
    #
    # Returns a String.
    def adapter_stack
      if respond_to?(:adapter) && adapter
        "#{name} -> #{adapter.adapter_stack}"
      else
        name.to_s
      end
    end

    private

    # Internal: Raises for feature names reserved for Flipper's own data. Adapters
    # that store that data next to features call it before every write.
    def ensure_not_reserved_key!(feature)
      raise ReservedKeyError.new(feature.key) if Flipper.reserved_key?(feature.key)
    end

    # Internal: Normalizes a gate value (Set, Array, nil, or anything that
    # responds to #to_set or #to_a) into a Set of String member values.
    #
    # Returns a Set of Strings.
    def normalize_member_values(value)
      values = case value
               when nil
                 Set.new
               when Set
                 value
               when Array
                 value.to_set
               else
                 value.respond_to?(:to_set) ? value.to_set : value.to_a.to_set
               end

      values.map(&:to_s).to_set
    end

    # Internal: Normalizes an optional caller-observed member list. Accepts
    # Strings, Symbols, Actors and Types::Actor instances.
    #
    # Returns nil when the caller did not supply an expectation, so callers can
    # distinguish "no expectation" from "expected an empty set".
    def normalize_expected_members(expected)
      return nil if expected.nil?

      unless expected.respond_to?(:each)
        raise ArgumentError, "expected must be enumerable, but was #{expected.inspect}"
      end

      expected.each_with_object(Set.new) do |member, set|
        set << member_value(member)
      end
    end

    # Internal: Converts anything actor-ish to the String stored by adapters.
    #
    # Returns a String.
    def member_value(member)
      return member.to_s if member.is_a?(String) || member.is_a?(Symbol)
      return member.flipper_id.to_s if member.respond_to?(:flipper_id)
      return member.value.to_s if member.respond_to?(:value)

      raise ArgumentError, "#{member.inspect} is not an actor or member value"
    end

    # Internal: Decides what a replacement should do without writing anything.
    #
    # current     - Set of the member values the adapter reports now.
    # old_value   - String value that should leave the set.
    # new_value   - String value that should join the set.
    # expected    - Normalized Set the caller observed, or nil for none.
    # feature_key - Used for error messages.
    #
    # Returns the Set the gate should contain after the replacement. May equal
    # current when the replacement is already the case.
    #
    # Raises Flipper::ActorSetConflict when the observed members are stale, or
    # when neither the old nor the new member is enabled.
    def plan_member_replacement(current, old_value:, new_value:, expected:, feature_key:)
      old_value = old_value.to_s
      new_value = new_value.to_s
      expected = normalize_expected_members(expected)

      if expected && expected != current
        raise ActorSetConflict.new(
          feature_key: feature_key,
          expected: expected,
          actual: current,
        )
      end

      unless current.include?(old_value) || current.include?(new_value)
        raise ActorSetConflict.new(
          "Cannot replace member #{old_value.inspect} with #{new_value.inspect} on feature " \
            "#{feature_key.inspect} because #{old_value.inspect} is not enabled. " \
            "Current members: #{current.to_a.sort.inspect}. " \
            "Re-read the members and retry if another replacement already ran.",
          feature_key: feature_key,
          expected: expected,
          actual: current,
        )
      end

      prospective = current.dup
      prospective.delete(old_value)
      prospective.add(new_value) unless new_value.empty?
      prospective
    end
  end
end

require "set"
require "flipper/exporter"
require "flipper/feature"
require "flipper/adapters/sync/synchronizer"
