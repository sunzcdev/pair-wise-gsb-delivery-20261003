# frozen_string_literal: true

module Dry
  class Container
    # Carries the active scope and the current construction chain through a
    # (possibly nested) resolution.
    #
    # A single context is shared for all items resolved from one container
    # because its state is stored fiber-locally via the {ScopeManager}.
    #
    # @api private
    class ResolutionContext
      # @return [Dry::Container::ScopeManager]
      attr_reader :manager

      # @api private
      def initialize(manager)
        @manager = manager
      end

      # @return [Dry::Container::Scope, nil]
      def current_scope
        manager._scopes.last
      end

      # Resolve an item while enforcing lifecycle rules
      #
      # @param [Dry::Container::Item] item
      # @param [String] key
      #
      # @return [Mixed]
      def resolve(item, key)
        frames = manager._frames

        if item.lifecycle == :scope && frames.any? { |frame| frame[:lifecycle] == :application }
          raise_scope_leak!(frames, key)
        end

        case item.lifecycle
        when :scope
          scope = current_scope

          if scope.nil?
            raise ScopeError,
                  "cannot resolve #{key.inspect} with lifecycle :scope " \
                  "outside of an active scope"
          end

          scope.memoize(key) do
            construct(item, key)
          end
        else
          item.resolve(self, key)
        end
      end

      # Run an item factory as a single frame on the construction chain
      #
      # @param [Dry::Container::Item] item
      # @param [String] key
      #
      # @return [Mixed]
      def construct(item, key)
        frames = manager._frames

        frames << {key: key, lifecycle: item.lifecycle}

        begin
          item.callable? ? item.item.call : item.item
        ensure
          frames.pop
        end
      end

      private

      # @private
      def raise_scope_leak!(frames, key)
        chain = frames.map { |frame|
          "#{frame[:key].inspect} (#{frame[:lifecycle]})"
        }
        chain << "#{key.inspect} (scope)"

        raise ScopeLeakError,
              "application-scoped object cannot capture a scope-scoped " \
              "dependency: #{chain.join(" -> ")}"
      end
    end
  end
end
