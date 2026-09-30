# frozen_string_literal: true

module Dry
  module Monads
    # Control markers and shared machinery for stack-safe tail-recursive
    # loops over monadic steps.
    #
    # `tail_rec` repeatedly runs a step function starting from an initial
    # state. Every step returns a monad wrapping a control marker:
    # Continue(next_state) to keep looping or Done(value) to stop with
    # a final value. The loop is implemented as a trampoline so it doesn't
    # consume the Ruby call stack no matter how many iterations it takes.
    # This makes it a good fit for things like paginated synchronization
    # where a naive `bind`-based recursion would blow the stack.
    #
    # @example
    #   include Dry::Monads[:result]
    #
    #   Dry::Monads::Result.tail_rec(cursor: nil, acc: []) do |state|
    #     fetch_page(state[:cursor]).fmap do |page|
    #       acc = state[:acc] + page.items
    #       page.next_cursor ? Continue(cursor: page.next_cursor, acc: acc) : Done(acc)
    #     end
    #   end
    #
    # @api public
    module TailRec
      # Marker telling tail_rec to run another iteration with a new state.
      #
      # @api public
      class Continue
        include ::Dry::Equalizer(:state)

        # @return [Object] state passed to the next iteration
        attr_reader :state

        # @param state [Object] state for the next iteration
        def initialize(state)
          @state = state
        end

        # @return [String]
        def to_s = "Continue(#{@state.inspect})"
        alias_method :inspect, :to_s
      end

      # Marker telling tail_rec to stop looping and return a final value.
      #
      # @api public
      class Done
        include ::Dry::Equalizer(:value)

        # @return [Object] final value of the loop
        attr_reader :value

        # @param value [Object] final value of the loop
        def initialize(value)
          @value = value
        end

        # @return [String]
        def to_s = "Done(#{@value.inspect})"
        alias_method :inspect, :to_s
      end

      # Checks that a value produced by a step is a proper control marker.
      #
      # @param step [Object] unwrapped value returned by a tail_rec step
      # @return [Continue, Done]
      # @raise [InvalidTailRecStepError] if the value isn't a control marker
      #
      # @api private
      def self.check_step!(step)
        if step.is_a?(Continue) || step.is_a?(Done)
          step
        else
          raise InvalidTailRecStepError, step
        end
      end

      # Value constructors for loop control markers.
      #
      # @api public
      module Constructors
        # Continue constructor
        #
        # @param state [Object] state for the next iteration
        # @return [TailRec::Continue]
        def Continue(state) = TailRec::Continue.new(state)

        # Done constructor
        #
        # @param value [Object] final value of the loop
        # @return [TailRec::Done]
        def Done(value) = TailRec::Done.new(value)
      end
    end

    extend TailRec::Constructors
  end
end
