# frozen_string_literal: true

require "dry/container/error"

module Dry
  class Container
    # Default resolver for resolving items from container
    #
    # @api public
    class Resolver
      # Resolve an item from the container
      #
      # @param [Concurrent::Hash] container
      #   The container
      # @param [Mixed] key
      #   The key for the item you wish to resolve
      # @yield
      #   Fallback block to call when a key is missing. Its result will be returned
      # @yieldparam [Mixed] key Missing key
      #
      # @raise [KeyError]
      #   If the given key is not registered with the container (and no block provided)
      #
      #
      # @return [Mixed]
      #
      # @api public
      def call(container, key, &block)
        target = key.to_s
        item = container.fetch(target) do
          if block
            return yield(key)
          else
            raise KeyError.new(
              %(key not found: "#{key}"),
              key: target,
              receiver: container
            )
          end
        end

        if dependency_graph_item?(item)
          Activation.new(container, target, item).call
        else
          item.call
        end
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

      private

      # @private
      def dependency_graph_item?(item)
        item.respond_to?(:dependencies) &&
          (!item.dependencies.empty? ||
            (item.respond_to?(:lazy_dependencies) &&
              !item.lazy_dependencies.empty?))
      end

      # Validates and activates exactly the part of the dependency graph
      # reachable from one resolve target
      #
      # @api private
      class Activation
        # @private
        def initialize(container, target, item)
          @container = container
          @target = target
          @item = item
          @instances = {}
          @lazy = {}
          @items = {}
        end

        # Validate the eager chain, then activate only it
        #
        # @return [Mixed]
        #
        # @private
        def call
          complete_eager_closure(@target, @item)
          ensure_eager_acyclic
          activate(@target, @item)
        end

        private

        # Walk the eager edges reachable from +root+, registering every item
        # that may have to be activated. Lazy edges are deliberately ignored:
        # their existence and chain are only checked once the factory calls
        # the injected callable.
        #
        # @private
        def complete_eager_closure(root, root_item)
          pending = [[root, root_item, [root]]]

          while (key, item, path = pending.shift)
            next if @items.key?(key)

            @items[key] = item

            eager_dependencies(item).each do |_name, dependency_key|
              dependency = @container[dependency_key]

              if dependency.nil?
                raise MissingDependencyError,
                      "Missing dependency #{dependency_key.inspect} " \
                      "required from #{@target.inspect}: " \
                      "#{(path + [dependency_key]).join(" -> ")}"
              end

              pending << [dependency_key, dependency, path + [dependency_key]]
            end
          end
        end

        # Reject cycles made entirely of non-lazy dependencies. Cycles broken
        # by at least one lazy edge are allowed because the lazy dependency is
        # only resolved after the factory has run.
        #
        # @private
        def ensure_eager_acyclic
          colors = Hash.new(:white)
          stack = []

          visit = lambda { |key|
            return if colors[key] == :black

            if colors[key] == :gray
              cycle = stack.drop_while { |pending| pending != key } + [key]
              raise CircularDependencyError,
                    "Circular dependency detected: #{cycle.join(" -> ")}"
            end

            colors[key] = :gray
            stack.push(key)

            eager_dependencies(@items.fetch(key)).each do |_, dependency_key|
              visit.call(dependency_key)
            end

            stack.pop
            colors[key] = :black
          }

          visit.call(@target)
        end

        # Build one instance of each eagerly required dependency, depth first
        #
        # @private
        def activate(key, item)
          @instances.fetch(key) do
            resolved = eager_dependencies(item).to_h { |_name, dependency_key|
              [dependency_key, activate(dependency_key, @items.fetch(dependency_key))]
            }

            thunks = lazy_dependencies(item).to_h { |_name, dependency_key|
              [dependency_key, lazy_thunk(dependency_key)]
            }

            value =
              if dependencies_declared?(item)
                item.call(**merge_dependency_values(item, resolved, thunks))
              else
                item.call
              end

            @instances[key] = value
          end
        end

        # @private
        def lazy_thunk(key)
          @lazy.fetch(key) do
            @lazy[key] = -> { resolve_lazy(key) }
          end
        end

        # Validate and activate a dependency when its callable is invoked.
        # The check is postponed this far so that lazy dependencies may be
        # registered only after the target factory has run.
        #
        # @private
        def resolve_lazy(key)
          item = @container.fetch(key) do
            raise MissingDependencyError,
                  "Missing lazy dependency #{key.inspect} " \
                  "(required lazily from #{@target.inspect})"
          end

          complete_eager_closure(key, item)
          ensure_eager_acyclic
          activate(key, item)
        end

        # @private
        def eager_dependencies(item)
          item.respond_to?(:dependencies) ? item.dependencies : []
        end

        # @private
        def lazy_dependencies(item)
          item.respond_to?(:lazy_dependencies) ? item.lazy_dependencies : []
        end

        # @private
        def dependencies_declared?(item)
          !eager_dependencies(item).empty? || !lazy_dependencies(item).empty?
        end

        # @private
        def merge_dependency_values(item, resolved, thunks)
          values = {}
          eager_dependencies(item).each { |name, key| values[name.to_sym] = resolved.fetch(key) }
          lazy_dependencies(item).each { |name, key| values[name.to_sym] = thunks.fetch(key) }
          values
        end
      end
    end
  end
end
