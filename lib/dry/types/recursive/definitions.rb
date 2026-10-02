# frozen_string_literal: true

module Dry
  module Types
    module Recursive
      # A registry of named recursive types.
      #
      # References can be requested with {#[]} before the corresponding body
      # is assigned - the body is only resolved on the first type
      # application. After the definition block runs, {#finalize!} verifies
      # that every referenced name was given a body, i.e. that the recursion
      # is closed.
      #
      # @api public
      class Definitions
        # @return [Hash{Symbol => Reference}]
        attr_reader :references

        # @return [Hash{Symbol => Type}]
        attr_reader :bodies

        # @api private
        def initialize
          @references = {}
          @bodies = {}
          @finalized = false
          @resolved = {}
        end

        # Fetch (and memoize) a named reference.
        #
        # @param [Symbol, String] name
        #
        # @return [Reference]
        #
        # @api public
        def [](name)
          reference!(name)
        end

        # Fetch (and memoize) a named reference.
        #
        # @param [Symbol, String] name
        #
        # @return [Reference]
        #
        # @api private
        def reference!(name)
          name = name.to_sym
          references[name] ||= Reference.new(self, name)
        end

        # Assign the body of a named recursive type.
        #
        # @param [Symbol, String] name
        # @param [Type] type
        #
        # @return [Type]
        #
        # @api public
        def []=(name, type)
          define!(name, type)
        end

        # Assign the body of a named recursive type.
        #
        # @param [Symbol, String] name
        # @param [Type] type
        #
        # @return [Type]
        #
        # @api public
        def define!(name, type)
          name = name.to_sym

          unless type.is_a?(Type)
            raise ::ArgumentError,
              "#{type.inspect} is not a Dry::Types type, cannot be assigned to #{name.inspect}"
          end

          if bodies.key?(name)
            raise RecursiveDefinitionError,
              "recursive type #{name.inspect} is already defined"
          end

          bodies[name] = type
        end
        alias_method :define, :define!

        # Resolve a reference to its body.
        #
        # @param [Symbol, String, Reference] reference
        #
        # @return [Type]
        #
        # @raise [RecursiveDefinitionError] when the group was not finalized
        # @raise [UnresolvedReferenceError] when the name has no body
        #
        # @api public
        def resolve(reference)
          name = reference.is_a?(Reference) ? reference.name : reference.to_sym

          @resolved[name] ||= begin
            unless @finalized
              raise RecursiveDefinitionError,
                "recursive group is not finalized, call #finalize! before applying types"
            end

            bodies.fetch(name) do
              raise UnresolvedReferenceError, name
            end
          end
        end

        # Check that every referenced name has a body.
        #
        # @return [void]
        #
        # @raise [UnresolvedReferenceError]
        #
        # @api public
        def finalize!
          return self if @finalized

          missing = references.keys - bodies.keys

          unless missing.empty?
            raise UnresolvedReferenceError, missing.sort
          end

          @finalized = true
          self
        end

        # @return [Boolean]
        #
        # @api public
        def finalized?
          @finalized
        end

        # Wrap a root reference in a {Scope} carrying the whole group so it
        # can be exported to AST and rebuilt later.
        #
        # @param [Symbol, String] root
        #
        # @return [Scope]
        #
        # @api public
        def scope(root)
          root = root.to_sym

          unless bodies.key?(root)
            raise UnresolvedReferenceError, root
          end

          Scope.new(self[root], self, root)
        end

        # Iterate over the defined names in assignment order.
        #
        # @return [Array<Symbol>]
        #
        # @api private
        def names
          bodies.keys
        end

        # Evaluate a definition block with the registry available both as a
        # block argument and as +self+.
        #
        # @return [void]
        #
        # @api private
        def evaluate(&block)
          if block.arity.zero?
            # instance_exec rebinds self to the registry even for blocks
            # defined at the top level (whose lexical self is +main+).
            instance_exec(&block)
          else
            block.(self)
          end
        end

        private

        # Allows bare reference access (e.g. +foo+) inside instance_eval
        # definition blocks.
        #
        # @api private
        def method_missing(name, *args, &block)
          if args.empty? && !block && !name.end_with?("=")
            self[name]
          else
            super
          end
        end

        # @api private
        def respond_to_missing?(name, include_private = false)
          (!name.end_with?("=") && references.key?(name) || bodies.key?(name)) || super
        end
      end
    end
  end
end