# frozen_string_literal: true

module Dry
  class Container
    # An alias entry pointing at another key in the container.
    #
    # An alias does not hold its own factory: resolution always follows the
    # chain until a "real" registered item is reached, so every name in the
    # chain shares the same resolved object and its memoization state.
    #
    # Decorators scoped to this alias entry alone are stored here and only
    # affect resolution through this entry; they never touch the terminal
    # registration or other aliases pointing at it.
    #
    # @api public
    class Alias
      # @return [String] the key this alias points at
      attr_reader :target

      # @return [Array<#call>] decorators applied only when resolving through
      #   this alias entry
      attr_reader :decorators

      # @param [Mixed] target
      #   The key this alias points at
      #
      # @return [Dry::Container::Alias]
      #
      # @api public
      def initialize(target)
        @target = target.to_s.dup.freeze
        @decorators = []
      end

      # Build a copy with an alias-scoped decorator appended
      #
      # @param [#call] decorator
      #
      # @return [Dry::Container::Alias]
      #
      # @api private
      def decorate(decorator)
        self.class.new(target).tap do |aliass|
          aliass.decorators.concat(decorators)
          aliass.decorators << decorator
        end
      end

      # Build a copy whose target is prefixed with a namespace
      #
      # @param [String] namespace
      # @param [String] separator
      #
      # @return [Dry::Container::Alias]
      #
      # @api private
      def namespaced(namespace, separator)
        self.class.new([namespace, target].join(separator)).tap do |aliass|
          aliass.decorators.concat(decorators)
        end
      end
    end
  end
end
