# frozen_string_literal: true

module Dry
  module Validation
    # API for registering and accessing Rule macros
    #
    # @api public
    module Macros
      module Registrar
        # Register a macro
        #
        # @example register a global macro
        #   Dry::Validation.register_macro(:even_numbers) do
        #     key.failure('all numbers must be even') unless values[key_name].all?(&:even?)
        #   end
        #
        # @example register a contract macro
        #   class MyContract < Dry::Validation::Contract
        #     register_macro(:even_numbers) do
        #       key.failure('all numbers must be even') unless values[key_name].all?(&:even?)
        #     end
        #   end
        #
        # @param [Symbol] name The name of the macro
        # @param [Array] args Optional default positional arguments for the macro
        #
        # @return [self]
        #
        # @see Macro
        #
        # @api public
        def register_macro(...)
          macros.register(...)
          self
        end
      end

      # Registry for macros
      #
      # @api public
      class Container
        include Core::Container::Mixin

        # Register a new macro
        #
        # @example in a contract class
        #   class MyContract < Dry::Validation::Contract
        #     register_macro(:even_numbers) do
        #       key.failure('all numbers must be even') unless values[key_name].all?(&:even?)
        #     end
        #   end
        #
        # @example with a signature describing its arguments
        #   class MyContract < Dry::Validation::Contract
        #     register_macro(:min, signature: {required: [:num]}) do |macro:|
        #       key.failure("must have at least #{macro.args[0]} items") if value.size < macro.args[0]
        #     end
        #   end
        #
        # @param [Symbol] name The name of the macro
        # @param [Hash, Macro::Signature] signature Optional signature used to
        #   verify macro calls when a contract class is finalized
        #
        # Registering a macro with an already used name replaces the previous
        # macro, which allows subclasses to override inherited macros
        #
        # @return [self]
        #
        # @api public
        def register(name, *args, signature: nil, &block)
          macro = Macro.new(
            name, args: args, block: block, signature: signature && Macro::Signature[signature]
          )
          _container.delete(name.to_s) if key?(name)
          super(name, macro, call: false, &nil)
          self
        end
      end

      # Return a registered macro
      #
      # @param [Symbol] name The name of the macro
      #
      # @return [Proc]
      #
      # @api public
      def self.[](name)
        container[name]
      end

      # Register a global macro
      #
      # @see Container#register
      #
      # @return [Macros]
      #
      # @api public
      def self.register(...)
        container.register(...)
        self
      end

      # @api private
      def self.container
        @container ||= Container.new
      end
    end

    # Acceptance macro
    #
    # @api public
    Macros.register(:acceptance) do
      key.failure(:acceptance, key: key_name) unless values[key_name].equal?(true)
    end
  end
end
