require "flipper/errors"
require "flipper/expressions/feature_enabled"

module Flipper
  module Adapters
    # An adapter for migrating a managed set of flags onto a new storage backend
    # while some of the flags they reference stay behind in the old backend.
    #
    # Managed flags and their ordinary local gate rules are served from the
    # local adapter. References made *from* a managed flag to another feature
    # (the FeatureEnabled/FeatureDisabled expression functions) are only
    # resolved against another adapter when an explicit declaration allows it.
    #
    # Declarations are per edge:
    #
    #   Flipper::Adapters::Federated.new(
    #     local,
    #     external: {
    #       legacy: legacy_adapter
    #     },
    #     dependencies: {
    #       # The :checkout_v2 flag is allowed to pull :search from :legacy.
    #       "checkout_v2" => { "search" => :legacy }
    #     }
    #   )
    #
    # With that declaration, evaluating the reference inside checkout_v2 reads
    # :search from the legacy adapter, so changing :search there changes the
    # result even though checkout_v2 itself lives in the local adapter. A
    # reference to a feature that is not declared for the referencing feature
    # raises Flipper::MissingDependency instead of silently querying any
    # adapter, so an undeclared external dependency cannot go unnoticed.
    class Federated
      include ::Flipper::Adapter

      # Internal: The thread-local key holding the stack of features whose
      # rules are currently being interpreted. Flipper::Expressions::
      # FeatureEnabled pushes the referencing feature before resolving a
      # reference, so the last entry is the origin of any nested read.
      ORIGIN_STACK_KEY = Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY

      attr_reader :local, :external, :dependencies

      # Public: Build a new federated adapter.
      #
      # local        - The adapter holding the managed, migrating flags.
      # external     - Hash of name => adapter for the backends that still hold
      #                flags left behind during the migration.
      # dependencies - Hash of managed feature name => Hash of referenced
      #                feature name => external adapter name. Only these edges
      #                may leave the local adapter.
      def initialize(local, external:, dependencies:)
        @local = local
        @external = external.each_with_object({}) do |(name, adapter), memo|
          memo[name.to_s] = adapter
        end
        @dependencies = normalize_dependencies(dependencies)
        @external_dsls = @external.each_with_object({}) do |(name, adapter), memo|
          memo[name] = Flipper.new(adapter)
        end
      end

      def adapter_stack
        sources = @external.map { |name, adapter| "#{name}: #{adapter.adapter_stack}" }.join(", ")
        "federated(local: #{@local.adapter_stack}, external: {#{sources}})"
      end

      def read_only?
        @local.read_only?
      end

      def features
        @local.features
      end

      def get(feature)
        origin = origin_stack.last

        # A read with no origin on the stack is an ordinary read of a managed
        # flag's own gate values, which always comes from the local adapter.
        return @local.get(feature) if origin.nil?

        target = source_for(origin, feature.key)

        if target.nil?
          # The reference has no declaration for this origin. Do not query any
          # adapter automatically; report the missing dependency instead.
          raise MissingDependency.new(origin, feature.key)
        end

        @external_dsls.fetch(target).adapter.get(feature)
      end

      def get_multi(features)
        # Each feature may route differently, so resolve them one at a time
        # rather than asking a single backend for the whole batch.
        features.each_with_object({}) do |feature, memo|
          memo[feature.key] = get(feature)
        end
      end

      def get_all(**kwargs)
        @local.get_all(**kwargs)
      end

      def sync_version
        @local.sync_version
      end

      def sync_version=(version)
        @local.sync_version = version
      end

      def add(feature)
        @local.add(feature)
      end

      def remove(feature)
        @local.remove(feature)
      end

      def clear(feature)
        @local.clear(feature)
      end

      def enable(feature, gate, thing)
        @local.enable(feature, gate, thing)
      end

      def disable(feature, gate, thing)
        @local.disable(feature, gate, thing)
      end

      private

      def source_for(origin, dependency_name)
        return nil if origin.nil?
        @dependencies.dig(origin.to_s, dependency_name.to_s)
      end

      def origin_stack
        Thread.current[ORIGIN_STACK_KEY] ||= []
      end

      def normalize_dependencies(dependencies)
        dependencies.each_with_object({}) do |(feature, edges), memo|
          memo[feature.to_s] = edges.each_with_object({}) do |(dependency, source), edge_memo|
            source_name = source.to_s
            unless @external.key?(source_name)
              raise ArgumentError, "Unknown external source #{source.inspect} for dependency #{dependency.inspect} of #{feature.inspect}. Known sources: #{@external.keys.inspect}"
            end

            edge_memo[dependency.to_s] = source_name
          end
        end
      end
    end
  end
end
