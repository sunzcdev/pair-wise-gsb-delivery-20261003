# frozen_string_literal: true

module Dry
  module Types
    module Recursive
      # Runtime guard tracking active recursive references and the current
      # position in the input value.
      #
      # The state is stored thread-locally so independent top-level calls
      # never observe each other, and every mutation is paired with an
      # +ensure+ cleanup.
      #
      # Position tracking is *attempt based*: every child type application
      # ({#attempt}) opens a checkpoint at the current path length. If the
      # attempt fails (a Sum falls back to another alternative, a schema
      # rejects a key, etc.) all path steps it contributed are rolled back
      # immediately. Hence an error always reports the position of the
      # value that was actually tried, never a rejected sibling.
      #
      # Two distinct situations are detected:
      #
      # * a reference visits the same object after consuming at least one
      #   key or index - a cyclic input value ({CyclicInputError})
      # * a reference visits the exact same object without moving in the
      #   input - a non-productive definition ({NonProductiveRecursionError})
      #
      # @api private
      class Guard
        # @api private
        Frame = ::Struct.new(:name, :id, :path_size)

        # @api private
        State = ::Struct.new(:frames, :path)

        # @api private
        NOOP_REJECT = proc {}.freeze

        # @return [Symbol]
        attr_reader :state_key

        # @api private
        def initialize
          @state_key = :"dry_types_recursive_guard_#{object_id}"
        end

        # Execute a guarded recursive type application.
        #
        # Nested frames run the body untouched. The outermost frame owns the
        # thread-local state and, for safe/+try+ calls, converts a cyclic
        # value into a failure only here - after every recursive frame has
        # unwound - to keep the error path stack short.
        #
        # @param [Reference] reference
        # @param [Object] input
        # @param [Boolean] safe
        # @param [#call, nil] fallback
        # @param [Boolean] try_mode
        #
        # @return [Object]
        #
        # @api private
        def call(reference, input, safe: false, fallback: nil, try_mode: false, try_block: nil)
          state = current_state
          top_level = state.nil?

          Thread.current[@state_key] = State.new([], []) if top_level
          state = current_state

          enter(state, reference, input)
          pushed = true

          yield
        rescue CyclicInputError => exception
          # Inner frames never convert: they propagate so Sum branches and
          # schema keys unwind (rolling their path steps back).
          raise unless top_level

          if try_mode
            failure = reference.send(:failure, input, exception)
            try_block ? try_block.call(failure) : failure
          elsif safe
            if fallback
              # Safe fallback follows the ordinary dry-types convention:
              # it receives the (here, original) input, not a Result.
              fallback.call(input)
            else
              raise
            end
          else
            raise
          end
        ensure
          state.frames.pop if pushed && state
          reset if top_level
        end

        # Apply one child type (schema key / array element).
        #
        # The path is extended by +step+ and restored to its original length
        # in +ensure+. Rollback on rejection is additionally driven by
        # {Sum} / schema fallbacks through {#attempt}, so even steps whose
        # +descend+ block has not unwound yet are hidden from siblings.
        #
        # @param [Symbol, Integer] step
        #
        # @return [Object]
        #
        # @api private
        def descend(step)
          state = current_state

          return yield unless state

          depth = state.path.size
          state.path << step
          begin
            yield
          ensure
            restore(state, depth)
          end
        end

        # Open a fallback checkpoint. The block receives a +reject+ callable
        # which rolls back every path step contributed after the checkpoint.
        #
        # @return [Object] the block result
        #
        # @api private
        def attempt
          state = current_state

          return yield(NOOP_REJECT) unless state

          checkpoint = state.path.size
          reject = proc { restore(state, checkpoint) }

          yield(reject)
        end

        # Render a position path as +$[:children][0][:op]+
        #
        # @param [Array<Symbol, Integer>] path
        #
        # @return [String]
        #
        # @api private
        def self.format_path(path)
          return "$" if path.empty?

          path.reduce("$") { |acc, step|
            step.is_a?(::Integer) ? "#{acc}[#{step}]" : "#{acc}[#{step.inspect}]"
          }
        end

        # Reset all state once a top-level call unwinds.
        #
        # @return [void]
        #
        # @api private
        def reset
          Thread.current[@state_key] = nil
        end

        # @return [Boolean]
        #
        # @api private
        def active?
          !current_state.nil?
        end

        private

        # @return [State, nil]
        #
        # @api private
        def current_state
          Thread.current[@state_key]
        end

        # Truncate the path to +length+.
        #
        # @param [State] state
        # @param [Integer] length
        #
        # @return [void]
        #
        # @api private
        def restore(state, length)
          state.path.slice!(length..)
        end

        # @param [State] state
        # @param [Reference] reference
        # @param [Object] input
        #
        # @return [void]
        #
        # @raise [NonProductiveRecursionError]
        # @raise [CyclicInputError]
        #
        # @api private
        def enter(state, reference, input)
          id = input.object_id

          # A genuine re-entry requires BOTH the same named reference and the
          # same object. A cycle through a differently named reference is a
          # normal productive recursion (e.g. pair -> item -> pair on a
          # different, nested value), so it must not match here.
          frame = state.frames.reverse_each.find { |f|
            f.name == reference.name && f.id.equal?(id)
          }

          if frame
            position = state.path.dup

            if position.size == frame.path_size
              raise NonProductiveRecursionError.new(reference.name, position)
            else
              cycle = position[frame.path_size..]
              raise CyclicInputError.new(reference.name, position, cycle)
            end
          end

          state.frames << Frame.new(reference.name, id, state.path.size)
        end
      end
    end
  end
end