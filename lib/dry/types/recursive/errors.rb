# frozen_string_literal: true

module Dry
  module Types
    module Recursive
      # Base error for everything related to recursive type definitions
      #
      # @api public
      class RecursiveError < ::StandardError; end

      # Raised when a named reference does not have a body after the
      # definition block finished, i.e. the recursion is not closed.
      #
      # @api public
      class UnresolvedReferenceError < RecursiveError
        # @return [Symbol]
        attr_reader :name

        # @return [Array<Symbol>]
        attr_reader :missing

        # @param [Symbol, Array<Symbol>] name one name or the full list of missing names
        #
        # @api public
        def initialize(name)
          if name.is_a?(::Array)
            @missing = name
            @name = nil
            super("recursive references are not closed: #{name.map(&:inspect).join(", ")}")
          else
            @name = name
            @missing = [name]
            super("recursive reference #{name.inspect} is not defined")
          end
        end
      end

      # Raised when a recursive group is used before it has been finalized
      # or finalized a second time in an invalid way.
      #
      # @api public
      class RecursiveDefinitionError < RecursiveError; end

      # Raised at runtime when a recursive definition consumes no input
      # before referring back to itself, e.g.
      #
      #   r.x = r.x
      #
      # Such a definition would never terminate for any input.
      #
      # @api public
      class NonProductiveRecursionError < RecursiveError
        # @return [Symbol]
        attr_reader :name

        # @return [Array<Symbol, Integer>]
        attr_reader :path

        # @param [Symbol] name
        # @param [Array<Symbol, Integer>] path
        #
        # @api public
        def initialize(name, path = EMPTY_ARRAY)
          @name = name
          @path = path
          location = path.empty? ? "" : " at #{Guard.format_path(path)}"
          super(
            "recursive type #{name.inspect} refers to itself without consuming " \
            "any input#{location}; such a definition cannot terminate"
          )
        end
      end

      # Raised at runtime when the input value itself contains a cycle,
      # i.e. the same object is reached again through a key or index.
      #
      # This is a {CoercionError} so safe coercion and +#try+ report it as a
      # failure rather than aborting the process.
      #
      # @api public
      class CyclicInputError < CoercionError
        # @return [Symbol]
        attr_reader :name

        # @return [Array<Symbol, Integer>]
        attr_reader :path

        # @return [Array<Symbol, Integer>]
        attr_reader :cycle_path

        # @param [Symbol] name
        # @param [Array<Symbol, Integer>] path
        # @param [Array<Symbol, Integer>] cycle_path
        #
        # @api public
        def initialize(name, path, cycle_path)
          @name = name
          @path = path
          @cycle_path = cycle_path

          super(
            "cyclic value passed to recursive type #{name.inspect} " \
            "at #{Guard.format_path(path)} (the value repeats itself " \
            "via #{Guard.format_path(cycle_path)})"
          )
        end
      end
    end
  end
end