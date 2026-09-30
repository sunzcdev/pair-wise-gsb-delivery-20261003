# frozen_string_literal: true

module Dry
  module Monads
    # Stack-safe tail recursion for monadic steps.
    #
    # A chain of {RightBiased::Right#bind} calls grows the Ruby call stack
    # with every iteration, which breaks on large inputs (e.g. synchronizing
    # thousands of pages). This module provides an iterative loop driven by
    # two control markers:
    #
    # - {TailRec::Continue} carries the state of the next iteration
    # - {TailRec::Done} carries the final value
    #
    # Both markers must be wrapped in the monad of the loop (Result, Maybe,
    # or Task). {Result::Failure}, {Maybe::None}, and rejected {Task}s stop
    # the loop with their usual short-circuiting semantics; they are never
    # swallowed. Returning a bare value (or a wrong monad) from a step raises
    # {TailRec::MarkerError}, a +TypeError+.
    #
    # @example paginated sync with Result
    #   include Dry::Monads[:result, :tail_rec]
    #
    #   def sync_all
    #     Result.tail_rec(first_page) do |page|
    #       response = fetch_page(page)
    #
    #       if response.last_page?
    #         Success(Done(response.items))
    #       else
    #         Success(Continue(response.next_page))
    #       end
    #     end
    #   end
    #
    # @api public
    module TailRec
      # Base class of the control markers.
      # Includes {::Dry::Equalizer} so markers compare by carried value
      # and support pattern matching.
      class Marker
        include ::Dry::Equalizer(:value!)

        # @return [Object] the carried value
        attr_reader :value

        # @param value [Object] the carried value
        def initialize(value = Unit)
          @value = value
        end

        # @return [Object] the carried value
        def value! = @value

        # Pattern matching
        #
        # @return [Array<Object>]
        # @api private
        def deconstruct
          Unit.equal?(@value) ? EMPTY_ARRAY : [@value]
        end

        # Pattern matching of hash-shaped values
        #
        # @return [Object, nil]
        # @api private
        def deconstruct_keys(keys)
          if @value.respond_to?(:deconstruct_keys)
            @value.deconstruct_keys(keys)
          else
            EMPTY_HASH
          end
        end

        # @return [String]
        def inspect = "#{self.class.name.split("::").last}(#{@value.inspect})"
        alias_method :to_s, :inspect
      end

      # Instructs the loop to continue with the given state.
      #
      # @api public
      class Continue < Marker
      end

      # Instructs the loop to stop and return the wrapped value.
      #
      # @api public
      class Done < Marker
      end

      # Raised when a step doesn't return {Continue} or {Done}
      # wrapped in the loop's monad.
      class MarkerError < ::TypeError
        # @param output [Object] the value returned by the step
        def initialize(output)
          super(
            "tail_rec expects the block to return a monad wrapping " \
            "Continue(state) or Done(value), got #{output.inspect}"
          )
        end
      end

      # @param output [Object]
      # @return [Continue, Done]
      # @raise [MarkerError] if +output+ is not a marker
      # @api private
      def self.expect_marker(output)
        case output
        when Continue, Done
          output
        else
          raise MarkerError, output
        end
      end

      # A module that can be included for easier access to the markers.
      #
      # @api public
      module Mixin
        # @see TailRec::Continue
        Continue = Continue
        # @see TailRec::Done
        Done = Done

        module Constructors
          # Builds a {Continue} marker
          #
          # @param value [Object] state for the next step
          # @return [Continue]
          def Continue(value = Unit) = Continue.new(value)

          # Builds a {Done} marker
          #
          # @param value [Object] the final value
          # @return [Done]
          def Done(value = Unit) = Done.new(value)
        end

        include Constructors
      end

      # Drives a synchronous, stack-safe loop (Result and Maybe).
      #
      # @param monad [RightBiased::Right, RightBiased::Left]
      #   a monad holding the initial state
      # @yield [Object] state of the current iteration
      # @yieldreturn [RightBiased::Right, RightBiased::Left]
      #   a monad wrapping a {Continue}/{Done} marker, or a left side
      # @return [RightBiased::Right, RightBiased::Left]
      # @api private
      def self.run(monad, &block)
        current = monad

        loop do
          output = current.bind(&block)
          return output if output.failure?

          marker = TailRec.expect_marker(output.value!)
          next_value = marker.value!

          return output.fmap { next_value } if marker.is_a?(Done)

          current = output.fmap { next_value }
        end
      end
    end

    extend TailRec::Mixin::Constructors

    # @see TailRec::Continue
    Continue = TailRec::Continue
    # @see TailRec::Done
    Done = TailRec::Done

    require "dry/monads/registry"
    register_mixin(:tail_rec, TailRec::Mixin)
  end
end
