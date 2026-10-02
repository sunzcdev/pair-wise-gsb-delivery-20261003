# frozen_string_literal: true

module Dry
  module Types
    class Recursive
      # Tracks recursive type applications within the current thread
      #
      # The guard detects input values that refer to themselves
      # (directly or transitively) and records the current position
      # (hash keys and array indices) so that errors can point at the
      # self-referencing value.
      #
      # @api private
      class Guard
        THREAD_KEY = :dry_types_recursive_guard

        class << self
          # Run a block within a recursion frame
          #
          # @param [Symbol] name recursive type name
          # @param [Object] input
          #
          # @return [Object]
          def guard(name, input)
            if (guard = current)
              guard.frame(name, input) { yield }
            else
              guard = new
              Thread.current[THREAD_KEY] = guard

              begin
                guard.frame(name, input) { yield }
              ensure
                Thread.current[THREAD_KEY] = nil
              end
            end
          end

          # Append a path segment (hash key or array index) to the
          # current position. No-op when no recursive type is applied.
          #
          # @param [Symbol, Integer] segment
          #
          # @return [Object]
          def at(segment)
            if (guard = current)
              guard.at(segment) { yield }
            else
              yield
            end
          end

          # @return [Guard, nil]
          def current = Thread.current[THREAD_KEY]
        end

        def initialize
          @frames = []
          @path = []
        end

        # @api private
        def frame(name, input)
          return yield unless input.is_a?(::Hash) || input.is_a?(::Array)

          object_id = input.object_id

          @frames.each do |frame_object_id, frame_path|
            next unless frame_object_id == object_id

            # Re-entering the very same position is not a cycle:
            # alternative branches (e.g. sum types) are tried
            # against the same input
            return yield if frame_path == @path

            raise RecursiveInputError.new(
              name,
              path: @path.dup,
              first_seen_path: frame_path
            )
          end

          @frames << [object_id, @path.dup]

          begin
            yield
          ensure
            @frames.pop
          end
        end

        # @api private
        def at(segment)
          @path << segment

          begin
            yield
          ensure
            @path.pop
          end
        end
      end
    end
  end
end
