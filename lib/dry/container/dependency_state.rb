# frozen_string_literal: true

require "concurrent/hash"

module Dry
  class Container
    # @api private
    #
    # Per-container bookkeeping for dependency-aware replacement:
    #
    # * explicit dependency relations declared with +depends_on:+
    # * monotonically increasing version tokens, bumped whenever an entry
    #   is (re)published
    # * consistent resolution: a memoized entry invalidated by a
    #   replacement is rebuilt against one coherent set of dependency
    #   versions, never a half-old/half-new mix
    class DependencyState
      # @api private
      #
      # Versions of keys observed by a single top-level resolution.
      # A nested resolution (e.g. a factory calling +container[:other]+)
      # shares the context of the resolution that triggered it.
      class ResolutionContext
        # @param [DependencyState] state
        def initialize(state)
          @state = state
          @pinned = {}
        end

        # Remember the version of +key+ observed *before* its item is
        # called, so a replacement happening during construction can be
        # detected afterwards.
        #
        # @return [void]
        def pin(key, version)
          @pinned[key] = version unless @pinned.key?(key)
        end

        # Whether any observed dependency was replaced while the
        # resolution this context belongs to was in progress.
        #
        # @return [Bool]
        def stale?
          @pinned.any? { |key, version| @state.token(key) != version }
        end
      end

      # Maximum number of times a single top-level resolution is retried
      # because its dependencies were replaced while it was being built.
      #
      # @api private
      MAX_RESOLVE_ATTEMPTS = 32

      # @api private
      EMPTY_DEPENDENCIES = [].freeze

      # @api private
      THREAD_KEY = :_dry_container_dependency_state

      # Serializes replacement transactions
      #
      # @return [Mutex]
      #
      # @api private
      attr_reader :mutex

      def initialize
        @tokens = ::Concurrent::Hash.new
        @edges = ::Concurrent::Hash.new
        @mutex = ::Mutex.new
        @token_mutex = ::Mutex.new
      end

      def initialize_copy(source)
        super

        source_tokens = source.instance_variable_get(:@tokens)
        source_edges = source.instance_variable_get(:@edges)

        @tokens = ::Concurrent::Hash.new.merge!(source_tokens)
        @edges = ::Concurrent::Hash.new
        source_edges.each { |key, deps| @edges[key] = deps.dup.freeze }
        @mutex = ::Mutex.new
        @token_mutex = ::Mutex.new
      end

      # Current version token for a key (0 when never published)
      #
      # @param [String] key
      #
      # @return [Integer]
      #
      # @api private
      def token(key)
        @tokens.fetch(key, 0)
      end

      # Atomically move +key+ to its next version
      #
      # @param [String] key
      #
      # @return [Integer] the new token
      #
      # @api private
      def bump(key)
        @token_mutex.synchronize do
          @tokens[key] = token(key) + 1
        end
      end

      # Record the explicit dependency relations of +key+
      #
      # @param [String] key
      # @param [Array<#to_s>] dependencies
      #
      # @return [void]
      #
      # @api private
      def publish_edges(key, dependencies)
        @edges[key] = dependencies.map { |dep| dep.to_s.freeze }.freeze
      end

      # @param [String] key
      #
      # @return [Array<String>]
      #
      # @api private
      def dependencies(key)
        @edges.fetch(key, EMPTY_DEPENDENCIES)
      end

      # All keys that (transitively) depend on +root+ according to the
      # explicit dependency edges, +root+ itself included.
      #
      # @param [String] root
      #
      # @return [Array<String>]
      #
      # @api private
      def dependent_closure(root)
        reached = {root => true}
        frontier = [root]

        until frontier.empty?
          current = frontier.pop

          @edges.each do |candidate, deps|
            next if reached.key?(candidate)

            if deps.include?(current)
              reached[candidate] = true
              frontier << candidate
            end
          end
        end

        reached.keys
      end

      # Resolve +key+ from +container+, retrying the whole resolution
      # whenever a replacement invalidates a dependency while it is being
      # built.
      #
      # @param [Dry::Container::Mixin] container
      # @param [Mixed] key
      #
      # @return [Mixed]
      #
      # @api private
      def resolve(container, key, &block)
        if current_context.nil?
          attempts = 0
          value = nil

          loop do
            attempts += 1
            if attempts > MAX_RESOLVE_ATTEMPTS
              raise Error,
                    "could not consistently resolve #{key.inspect}: its dependencies " \
                    "were replaced more than #{MAX_RESOLVE_ATTEMPTS} times while it " \
                    "was being constructed"
            end

            context = ResolutionContext.new(self)
            value = with_context(context) { resolve_once(container, key, &block) }

            break unless context.stale?
          end

          value
        else
          resolve_once(container, key, &block)
        end
      end

      private

      # @return [ResolutionContext, nil]
      def current_context
        contexts = Thread.current[THREAD_KEY]
        contexts && contexts[object_id]
      end

      # @return [Mixed]
      def with_context(context)
        contexts = (Thread.current[THREAD_KEY] ||= {})
        contexts[object_id] = context
        yield
      ensure
        Thread.current[THREAD_KEY]&.delete(object_id)
      end

      # Perform one (potentially inconsistent) resolution attempt
      #
      # @return [Mixed]
      def resolve_once(container, key, &block)
        resolver = container.config.resolver
        container_hash = container._container
        key_s = key.to_s

        # A custom resolver that does not know about #fetch opts out of
        # consistency instrumentation entirely
        unless resolver.respond_to?(:fetch)
          return resolver.call(container_hash, key, &block)
        end

        used_fallback = false
        item =
          if block
            resolver.fetch(container_hash, key) do |missing|
              used_fallback = true
              yield(missing)
            end
          else
            resolver.fetch(container_hash, key)
          end

        return item if used_fallback

        context = current_context

        # The key itself and every explicitly declared dependency is
        # pinned before the item runs, so a replacement happening while
        # it is being constructed is detected even when the factory never
        # resolves those dependencies through the container itself.
        context.pin(key_s, token(key_s))
        dependencies(key_s).each { |dep| context.pin(dep, token(dep)) }

        item.call
      end
    end
  end
end
