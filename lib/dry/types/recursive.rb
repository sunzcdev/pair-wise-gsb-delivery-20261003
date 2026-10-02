# frozen_string_literal: true

module Dry
  module Types
    # Recursive types built from named, lazily resolved references
    #
    # Use {Dry::Types.recursive} for a single (usually self-referencing) type
    # or {Dry::Types::Recursive.define} for a group of mutually referencing
    # types.
    #
    # @api public
    module Recursive
      # @return [Guard]
      #
      # @api private
      def self.guard
        @guard ||= Guard.new
      end

      # Build a group of named recursive types.
      #
      # The block receives a {Definitions} registry. References may be used
      # before the corresponding body is assigned. Once the block returns,
      # every referenced name must have a body, otherwise an
      # {UnresolvedReferenceError} is raised. This makes sure a type
      # definition never silently expands into infinity.
      #
      # @example mutually referencing types
      #   group = Dry::Types::Recursive.define do |r|
      #     r[:foo] = Types::Hash.schema(bar: r[:bar])
      #     r[:bar] = Types::Hash.schema(foo: r[:foo])
      #   end
      #
      #   group[:foo] # => #<Dry::Types[Recursive<foo>]>
      #
      # @overload define(root, &block)
      #   Build a (usually self-referencing) type and return its {Scope}.
      #
      #   @param [Symbol] root name of the root reference
      #   @yieldparam [Definitions] r
      #   @yieldreturn [Type] the root body
      #
      #   @return [Scope]
      #
      # @overload define(&block)
      #   Build a group of mutually referencing types.
      #
      #   @yieldparam [Definitions] r
      #
      #   @return [Definitions]
      #
      # @api public
      def self.define(root = nil, &block)
        raise ::ArgumentError, "a block is required" unless block

        definitions = Definitions.new

        if root
          root = root.to_sym
          body =
            if block.arity.zero?
              definitions.instance_exec(&block)
            else
              block.(definitions)
            end
          definitions[root] = body
          definitions.finalize!
          definitions.scope(root)
        else
          definitions.evaluate(&block)
          definitions.finalize!
          definitions
        end
      end
    end

    # Build recursive types from named, lazily resolved references.
    #
    # @example a self-referencing approval-condition tree
    #   condition = Dry::Types.recursive(:condition) do |r|
    #     leaf = Types::Hash.schema(
    #       op: Types::String.constrained(included_in: %w[eq gt lt]),
    #       field: Types::String,
    #       value: Types::Any
    #     )
    #
    #     node = Types::Hash.schema(
    #       op: Types::String.constrained(included_in: %w[and or]),
    #       children: Types::Array.of(r[:condition])
    #     )
    #
    #     leaf | node
    #   end
    #
    #   condition.(tree)
    #
    # @example mutually referencing types
    #   group = Dry::Types::Recursive.define do |r|
    #     r[:foo] = Types::Hash.schema(bar: r[:bar])
    #     r[:bar] = Types::Hash.schema(foo: r[:foo])
    #   end
    #
    # @see Recursive.define
    #
    # @param [Symbol, nil] root
    #
    # @return [Recursive::Scope, Recursive::Definitions]
    #
    # @api public
    def self.recursive(root = nil, &block)
      raise ::ArgumentError, "a block is required" unless block

      Recursive.define(root, &block)
    end
  end
end

require "dry/types/recursive/guard"
require "dry/types/recursive/errors"
require "dry/types/recursive/reference"
require "dry/types/recursive/definitions"
require "dry/types/recursive/scope"
