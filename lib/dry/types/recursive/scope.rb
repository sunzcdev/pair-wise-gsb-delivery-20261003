# frozen_string_literal: true

module Dry
  module Types
    module Recursive
      # A root {Reference} wrapped together with its {Definitions} group.
      #
      # The scope behaves exactly like the root type for coercion purposes
      # but its AST carries all named bodies, which makes exporting and
      # rebuilding recursive types possible.
      #
      # @api public
      class Scope
        include Type
        include Options
        include Meta
        include Builder
        include Printable

        include ::Dry::Equalizer(:root, :definitions, :options, :meta, inspect: false, immutable: true)

        # @return [Reference]
        attr_reader :root

        # @return [Definitions]
        attr_reader :definitions

        # @return [Symbol]
        attr_reader :root_name

        # @param [Reference] root
        # @param [Definitions] definitions
        # @param [Symbol] root_name
        #
        # @api public
        def initialize(root = nil, definitions = nil, root_name = nil, **options)
          super(**options)
          @root = root
          @definitions = definitions
          @root_name = root_name
          @__args__ = [root, definitions, root_name].freeze
          freeze
        end

        # @api private
        def call_unsafe(input)
          root.call_unsafe(input)
        end

        # @api private
        def call_safe(input, &block)
          root.call_safe(input, &block)
        end

        # @api public
        def try(input, &block)
          root.try(input, &block)
        end

        # @return [Type]
        #
        # @api public
        def default? = root.default?

        # @return [Boolean]
        #
        # @api public
        def constrained? = root.constrained?

        # @return [false]
        #
        # @api public
        def optional? = false

        # @return [String]
        #
        # @api public
        def to_s
          "Recursive(#{root_name})"
        end

        # Export the whole group.
        #
        # The same named body only appears once; recursive leaves point back
        # to it with +:recursive_ref+, so the AST is a finite tree and
        # {#to_ast} always terminates.
        #
        # @return [Array]
        #
        # @api public
        def to_ast(meta: true)
          members = definitions.names.map { |name|
            [name, definitions.bodies[name].to_ast(meta: meta)]
          }

          [:recursive, [root_name, members, meta ? self.meta : EMPTY_HASH]]
        end

        # @return [Scope]
        #
        # @api public
        def lax
          Lax.new(self)
        end
      end
    end
  end
end