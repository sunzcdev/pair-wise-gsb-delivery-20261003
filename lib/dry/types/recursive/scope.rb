# frozen_string_literal: true

module Dry
  module Types
    class Recursive
      # Definition scope for a group of named recursive types
      #
      # A scope ties together a set of named type definitions that can
      # reference themselves and each other through {Recursive}
      # references. References are resolved lazily, so definitions
      # never need to be unfolded upfront.
      #
      # @example
      #   scope = Dry::Types.recursive do |t|
      #     t.define(:leaf) { Dry::Types["nominal.hash"].schema(op: Dry::Types["strict.string"]) }
      #     t.define(:node) do
      #       Dry::Types["nominal.hash"].schema(
      #         children: Dry::Types["nominal.array"].of(t.ref(:condition))
      #       )
      #     end
      #     t.define(:condition) { t.ref(:leaf) | t.ref(:node) }
      #   end
      #
      #   scope[:condition].(op: "and", children: [{op: "eq"}])
      #
      # @see Dry::Types.recursive
      #
      # @api public
      class Scope
        include ::Dry::Core::Constants

        # @return [Hash{Symbol => Type}] defined types by name
        attr_reader :definitions

        def initialize
          @definitions = {}
          @references = {}
          @finalized = false
        end

        # Define a named type within the scope
        #
        # @param [Symbol] name
        # @param [Type] type the definition, unless a block is given
        # @yieldparam [Scope] scope the scope itself, for building references
        #
        # @raise [RecursiveDefinitionError]
        #
        # @return [Type] the defined type
        #
        # @api public
        def define(name, type = nil)
          if finalized?
            raise RecursiveDefinitionError,
                  "cannot define #{name.inspect}, the recursive scope is already finalized"
          end

          name = name.to_sym

          if @definitions.key?(name)
            raise RecursiveDefinitionError, "recursive type #{name.inspect} is already defined"
          end

          type = yield(self) if block_given?

          unless type.is_a?(Type)
            raise RecursiveDefinitionError,
                  "definition of recursive type #{name.inspect} must be " \
                  "a Dry::Types::Type, got #{type.inspect}"
          end

          @definitions[name] = type
        end

        # Build a lazy reference to a named type. The name does not
        # have to be defined yet, but it must be defined by the time
        # the scope is finalized.
        #
        # @param [Symbol] name
        #
        # @return [Recursive]
        #
        # @api public
        def ref(name) = @references[name.to_sym] ||= Recursive.new(name.to_sym, self)

        # Fetch a defined type by name
        #
        # @param [Symbol] name
        #
        # @return [Type]
        #
        # @api public
        def [](name) = resolve(name)

        # @param [Symbol] name
        #
        # @return [Boolean]
        #
        # @api public
        def key?(name) = @definitions.key?(name.to_sym)

        # Resolve a name to its definition
        #
        # @param [Symbol] name
        #
        # @return [Type]
        #
        # @raise [RecursiveDefinitionError] when the name is not defined
        #
        # @api public
        def resolve(name)
          @definitions.fetch(name.to_sym) do
            raise RecursiveDefinitionError,
                  "recursive type #{name.to_sym.inspect} is not defined in this scope"
          end
        end

        # Whether the scope was finalized
        #
        # @return [Boolean]
        #
        # @api public
        def finalized? = @finalized

        # Check that all references are defined and that recursion
        # always consumes input, then close the scope for further
        # definitions
        #
        # @return [Scope] self
        #
        # @raise [RecursiveDefinitionError]
        #
        # @api public
        def finalize!
          return self if finalized?

          check_closure!
          check_productivity!

          @finalized = true
          self
        end

        # Export the scope as a finite AST. Recursive references are
        # emitted by name, so the AST closes over the definitions and
        # can be compiled back into an equivalent scope.
        #
        # @param [Boolean] meta whether to include type metadata
        #
        # @return [Array]
        #
        # @api public
        def to_ast(meta: true)
          [
            :recursive_definitions,
            [@definitions.map { |name, type| [name, type.to_ast(meta: meta)] }]
          ]
        end

        private

        # @api private
        def check_closure!
          missing = @references.keys - @definitions.keys

          return if missing.empty?

          refs = missing.map(&:inspect).join(", ")

          raise RecursiveDefinitionError,
                "recursive scope is not closed: #{refs} referenced but never defined"
        end

        # Recursion must go through a constructor that consumes input
        # (a hash schema, an array member or a map). Cycles through
        # transparent types only (sums, constraints, constructors...)
        # would recurse without consuming any input and hang.
        #
        # @api private
        def check_productivity!
          graph = @definitions.transform_values { |type| unguarded_references(type) }
          state = {}
          stack = []

          walk = lambda do |name|
            case state[name]
            when :done
              next
            when :open
              cycle = (stack[stack.index(name)..] + [name]).map(&:inspect).join(" -> ")

              raise RecursiveDefinitionError,
                    "recursive definition #{cycle} never consumes input and would not terminate; " \
                    "recursion must go through a hash schema, an array member or a map"
            end

            state[name] = :open
            stack << name
            graph[name].each { |ref| walk.call(ref) }
            stack.pop
            state[name] = :done
          end

          graph.each_key { |name| walk.call(name) }
        end

        # Names referenced from the type without consuming input
        #
        # @api private
        def unguarded_references(type)
          case type
          when Recursive
            [type.name]
          when Sum, Intersection, Implication
            unguarded_references(type.left) | unguarded_references(type.right)
          when Constrained, Constructor, Lax, Default, Enum
            unguarded_references(type.type)
          else
            EMPTY_ARRAY
          end
        end
      end
    end
  end
end
