# frozen_string_literal: true

module Pundit
  # Inspects the signature of a policy or scope constructor without invoking
  # it. This lets Pundit distinguish between a constructor that was called with
  # the wrong number of arguments and a constructor that was called
  # correctly, but which raises an +ArgumentError+ from inside its own body.
  #
  # Pundit always constructs policies and scopes with two positional
  # arguments (+user+ and +record+) and no keyword arguments.
  #
  # @api private
  # @since v2.5.3
  class ConstructorSignature
    # Number of positional arguments Pundit passes to policies and scopes.
    ARGUMENT_COUNT = 2

    # @param klass [Class] the policy or scope class whose constructor is checked
    # @return [void]
    # @raise [InvalidConstructorError] if the constructor cannot accept the
    #   arguments Pundit calls it with
    def self.verify!(klass)
      new(klass).verify!
    end

    # @param klass [Class] the policy or scope class whose constructor is checked
    def initialize(klass)
      @klass = klass
    end

    # Checks whether the constructor can be called with two positional
    # arguments and no keywords.
    #
    # Constructors whose signatures cannot be introspected are assumed to be
    # callable, so that any error they raise is left untouched.
    #
    # @return [Boolean]
    def compatible?
      parameters = constructor_parameters
      return true unless parameters

      required = parameters.count { |type,| type == :req }
      accepts_remaining_arguments = parameters.any? { |type,| %i[opt rest].include?(type) }
      requires_keyword_arguments = parameters.any? { |type,| type == :keyreq }

      return false if required > ARGUMENT_COUNT
      return false if required < ARGUMENT_COUNT && !accepts_remaining_arguments

      !requires_keyword_arguments
    end

    # @return [void]
    # @raise [InvalidConstructorError] if {#compatible?} is false
    def verify!
      return if compatible?

      raise InvalidConstructorError,
        "Invalid #<#{@klass}> constructor is called: Pundit called " \
          "#{constructor_name} with #{ARGUMENT_COUNT} positional arguments and no " \
          "keyword arguments, but the constructor accepts #{signature_description}"
    end

    private

    # The method that actually gets invoked by `klass.new(...)`. A class may
    # define its own `new`, in which case its signature takes precedence over
    # `initialize`.
    #
    # @return [Method, UnboundMethod, nil]
    def constructor_method
      custom_new = @klass.method(:new)
      return custom_new unless custom_new.owner == Class

      @klass.instance_method(:initialize)
    rescue StandardError
      nil
    end

    # @return [Array<Array(Symbol, Symbol)>, nil]
    def constructor_parameters
      constructor_method&.parameters
    end

    # @return [String]
    def constructor_name
      constructor_method.is_a?(Method) ? "#{@klass}.new" : "#{@klass}#initialize"
    end

    # Renders the constructor parameters as a human readable signature.
    #
    # @return [String]
    def signature_description
      parameters = constructor_parameters || []
      return "no parameters" if parameters.empty?

      parameters.map { |type, name| parameter_description(type, name) }.join(", ")
    end

    # @param type [Symbol]
    # @param name [Symbol]
    # @return [String]
    def parameter_description(type, name)
      case type
      when :req then name.to_s
      when :opt then "optional #{name}"
      when :rest then name == :* ? "*" : "*#{name}"
      when :keyreq then "required keyword #{name}"
      when :key then "optional keyword #{name}"
      when :keyrest then name == :** ? "**" : "**#{name}"
      when :block then name == :& ? "&" : "&#{name}"
      else type.to_s
      end
    end
  end
end
