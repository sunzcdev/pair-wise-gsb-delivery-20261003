# frozen_string_literal: true

module Dry
  module Types
    class Constructor < Nominal
      # A constructor whose coercion function is a lossless numeric coercion
      # (see {Coercions::Lossless}).
      #
      # The lossless function always validates the *output* of the coercion
      # chain. To preserve this guarantee, any extra constructor attached with
      # {#constructor}/{#prepend} is composed *before* the lossless function,
      # so it is impossible for an outer constructor to replace a validated
      # value with a lossy one (e.g. turn 3.7 into 3 after the fact).
      #
      # A wrapper constructor (a block taking +|input, type|+) deliberately
      # takes over the call, as everywhere in dry-types; it is an explicit
      # escape hatch and is documented as such.
      #
      # @api public
      class Lossless < Constructor
        # Attach a constructor before the lossless coercion
        #
        # @param [#call, nil] new_fn
        # @param [Hash] options
        # @param [#call, nil] block
        #
        # @return [Lossless]
        #
        # @api public
        def constructor(new_fn = nil, **options, &block)
          next_fn = Function[new_fn || block]

          if next_fn.wrapper?
            self.class.wrapper_type.new(with(**options), fn: next_fn)
          else
            with(**options, fn: fn << next_fn)
          end
        end
        alias_method :append, :constructor
        alias_method :>>, :constructor

        # Attach a constructor before the lossless coercion
        #
        # @param [#call, nil] new_fn
        # @param [Hash] options
        # @param [#call, nil] block
        #
        # @return [Lossless]
        #
        # @api public
        def prepend(new_fn = nil, **options, &block)
          next_fn = Function[new_fn || block]

          if next_fn.wrapper?
            self.class.wrapper_type.new(with(**options), fn: next_fn)
          else
            with(**options, fn: fn << next_fn)
          end
        end
        alias_method :<<, :prepend
      end
    end
  end
end
