# frozen_string_literal: true

require "delegate"

module Dry
  class Container
    # Default resolver for resolving items from container
    #
    # @api public
    class Resolver
      # Per-resolution state shared with factory items
      #
      # @api private
      class Context
        # @return [Resolver]
        attr_reader :resolver

        # @return [Concurrent::Hash]
        attr_reader :container

        # @return [Object, nil] object responding to #[] that overrides keys
        attr_reader :overrides

        # Keys currently being instantiated (stack of the active chain)
        #
        # @return [Array<String>]
        attr_reader :stack

        # @api private
        def initialize(resolver, container, overrides = nil)
          @resolver = resolver
          @container = container
          @overrides = overrides
          @stack = []
        end

        # Look up an override (e.g. a stub) for the key
        #
        # @param [String] key
        #
        # @return [Array(Symbol, Mixed)] [:hit, value] or [:miss]
        #
        # @api private
        def override_for(key)
          if overrides && overrides.key?(key)
            [:hit, overrides[key]]
          else
            [:miss]
          end
        end

        # Resolve an eager dependency
        #
        # @param [String, Symbol] key
        #
        # @return [Mixed]
        #
        # @api private
        def fetch(key)
          resolver.resolve_within(container, key.to_s, self)
        end

        # Resolve a dependency that was handed over as a lazy reference:
        # its own chain is validated only now, at dereference time
        #
        # @param [String] key
        #
        # @return [Mixed]
        #
        # @api private
        def lazy_fetch(key)
          resolver.validate_chain!(container, key) unless stack.include?(key)
          fetch(key)
        end

        # Return a lazy reference to a dependency
        #
        # @param [String, Symbol] key
        #
        # @return [Dry::Container::Resolver::LazyReference]
        #
        # @api private
        def lazy(key)
          LazyReference.new(self, key.to_s)
        end
      end

      # A reference to a dependency that is resolved on first use instead
      # of during construction of its dependent
      #
      # @api public
      class LazyReference < ::Delegator
        # @api private
        def initialize(context, key)
          @context = context
          @key = key
        end

        # @return [String] the referenced key
        #
        # @api public
        def __key__
          @key
        end

        # @api private
        def __getobj__
          @resolved ||= @context.lazy_fetch(@key)
        end

        # @api private
        def __setobj__(_obj)
          raise NotImplementedError, "lazy references cannot be reassigned"
        end
      end

      # Resolve an item from the container
      #
      # Before the item is instantiated the whole chain of its explicitly
      # declared dependencies is validated: every dependency must be
      # registered and no eager cycle is allowed.
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      # @yield
      #   Fallback block to call when the key itself is missing. Its result
      #   will be returned
      # @yieldparam [Mixed] key Missing key
      #
      # @raise [KeyError]
      #   If the given key is not registered (and no block is given)
      # @raise [Dry::Container::DependencyError]
      #   If a transitive dependency is missing or the chain contains an
      #   eager cycle
      #
      # @return [Mixed]
      #
      # @api public
      def call(container, key, overrides = nil, &block)
        key = key.to_s

        if overrides && overrides.key?(key)
          return overrides[key]
        end

        unless container.key?(key)
          return yield(key) if block

          raise KeyError.new(%(key not found: "#{key}"), key: key, receiver: container)
        end

        validate_chain!(container, key)

        instantiate(container, key, Context.new(self, container, overrides))
      end

      # Resolve an already validated key while constructing its dependent
      #
      # @param [Concurrent::Hash] container
      # @param [String] key
      # @param [Context] context
      #
      # @return [Mixed]
      #
      # @api private
      def resolve_within(container, key, context)
        if context.stack.include?(key)
          cycle = context.stack[context.stack.index(key)..] + [key]
          raise DependencyError,
                "Circular dependency detected: #{cycle.join(" -> ")}"
        end

        status, value = context.override_for(key)
        return value if status == :hit

        instantiate(container, key, context)
      end

      # Check whether an items is registered under the given key
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key you wish to check for registration with
      #
      # @return [Bool]
      #
      # @api public
      def key?(container, key)
        container.key?(key.to_s)
      end

      # An array of registered names for the container
      #
      # @return [Array]
      #
      # @api public
      def keys(container)
        container.keys
      end

      # Calls block once for each key in container, passing the key as a parameter.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return Hash
      #
      # @api public
      def each_key(container, &block)
        container.each_key(&block)
      end

      # Calls block once for each key in container, passing the key and
      # the registered item parameters.
      #
      # If no block is given, an enumerator is returned instead.
      #
      # @return Key, Value
      #
      # @api public
      # @note In discussions with other developers, it was felt that being able
      #       to iterate over not just the registered keys, but to see what was
      #       registered would be very helpful. This is a step toward doing that.
      def each(container, &block)
        container.map { |key, value| [key, value.call] }.each(&block)
      end

      # Instantiate a single key, threading the per-resolution context
      # through items that know how to use it
      #
      # @return [Mixed]
      #
      # @api private
      def instantiate(container, key, context)
        item = container.fetch(key)
        context.stack.push(key)

        begin
          if item.respond_to?(:dependencies) &&
             (item.dependencies.any? || item.lazy_dependencies.any?)
            item.call(context)
          else
            item.call
          end
        ensure
          context.stack.pop
        end
      end

      # Validate the reachable eager dependency chain of a target without
      # instantiating anything. Lazy dependencies must exist but their own
      # chains are left for dereference time.
      #
      # @raise [Dry::Container::DependencyError]
      #
      # @return [void]
      #
      # @api private
      def validate_chain!(container, target)
        # DFS frames: [key, actions left]; color: :visiting / :done
        # An action is [:eager, dep_key] or [:lazy, dep_key]
        colors = {target => :visiting}
        stack = [[target, actions_of(container, target)]]
        path = [target]

        until stack.empty?
          key, actions = stack.last

          if actions.empty?
            colors[key] = :done
            stack.pop
            path.pop
            next
          end

          kind, dep_key = actions.shift

          unless container.key?(dep_key)
            label = kind == :lazy ? " (lazy)" : ""
            raise DependencyError,
                  "Missing dependency #{dep_key.inspect} required by " \
                  "#{key.inspect}#{label} on chain " \
                  "#{(path + [dep_key]).join(" -> ")}"
          end

          # Lazy dependencies are resolved later, so their own chains are
          # not part of the eager chain of the current target
          next if kind == :lazy

          case colors[dep_key]
          when :visiting
            cycle_start = path.index(dep_key)
            cycle = path[cycle_start..] + [dep_key]
            raise DependencyError,
                  "Circular dependency detected: #{cycle.join(" -> ")}"
          when nil
            colors[dep_key] = :visiting
            path.push(dep_key)
            stack.push([dep_key, actions_of(container, dep_key)])
          end
        end
      end

      # @return [Array<Array(Symbol, String)>] actions ordered eager first
      #
      # @api private
      def actions_of(container, key)
        item = container.fetch(key)
        return [] unless item.respond_to?(:dependencies)

        item.dependencies.map { |dep| [:eager, dep] } +
          item.lazy_dependencies.map { |dep| [:lazy, dep] }
      end
    end
  end
end
