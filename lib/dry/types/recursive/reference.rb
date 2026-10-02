# frozen_string_literal: true

module Dry
  module Types
    module Recursive
      # A lazily resolved, named reference to another type in a
      # {Definitions} group.
      #
      # A reference is a full {Type} object so it can be placed in schemas,
      # arrays, sums, constructors, and so on while the group is still being
      # built. It only resolves its body on the first application, which is
      # what makes self and mutual references possible without expanding the
      # (infinite) structure ahead of time.
      #
      # @api public
      class Reference
        include Type
        include Options
        include Meta
        include Builder
        include Printable

        include ::Dry::Equalizer(:name, :definitions, :options, :meta, inspect: false, immutable: true)

        # @return [Symbol]
        attr_reader :name

        # @return [Definitions]
        attr_reader :definitions

        # @param [Definitions] definitions
        # @param [Symbol] name
        #
        # @api public
        def initialize(definitions = nil, name = nil, **options)
          super(**options)
          @definitions = definitions
          @name = name
          @__args__ = [definitions, name].freeze
          freeze
        end

        # @return [Type] the referenced type
        #
        # @api public
        def body
          definitions.resolve(name)
        end

        # @api private
        def call_unsafe(input)
          Recursive.guard.call(self, input) { body.call_unsafe(input) }
        end

        # @api private
        def call_safe(input, &block)
          # Cyclic values are converted into safe failures only at the
          # outermost guard boundary (see Guard#call), after the recursive
          # stack has unwound, so deep paths do not accumulate frames.
          Recursive.guard.call(self, input, safe: true, fallback: block) {
            body.call_safe(input, &block)
          }
        end

        # @api public
        def try(input, &block)
          Recursive.guard.call(self, input, try_mode: true, try_block: block) {
            body.try(input, &block)
          }
        end

        # @return [false]
        #
        # @api public
        def default? = false

        # @return [false] a reference cannot be known to be constrained
        #   without resolving its body, which must not happen while the
        #   group is still being built.
        #
        # @api public
        def constrained?
          false
        end

        # @return [false]
        #
        # @api public
        def optional? = false

        # @return [false] kept conservative for the same reason as
        #   {#constrained?}; the body is only applied at runtime.
        #
        # @api public
        def primitive?(_value)
          false
        end

        # @return [String]
        #
        # @api public
        def to_s
          "Recursive<#{name}>"
        end

        # @see Type#to_ast
        #
        # @api public
        def to_ast(meta: true)
          [:recursive_ref, [name, meta ? self.meta : EMPTY_HASH]]
        end

        # @return [Reference]
        #
        # @api public
        def lax
          Lax.new(self)
        end

        private

        # Build a {Result::Failure} carrying the recursive error
        #
        # @return [Result::Failure]
        #
        # @api private
        def failure(input, error)
          Result::Failure.new(input, error)
        end
      end
    end
  end
end