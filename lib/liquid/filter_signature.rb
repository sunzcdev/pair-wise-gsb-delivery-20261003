# frozen_string_literal: true

module Liquid
  class FilterSignature
    REQUIRED = Object.new.freeze

    INFINITY = Float::INFINITY

    def self.required
      REQUIRED
    end

    attr_reader :positional_arguments, :named_arguments, :aliases

    def self.build(positional_arguments: nil, named_arguments: nil, aliases: nil)
      new(
        positional_arguments: positional_arguments,
        named_arguments: named_arguments,
        aliases: aliases,
      )
    end

    def initialize(positional_arguments: nil, named_arguments: nil, aliases: nil)
      @positional_arguments = normalize_positional_arguments(positional_arguments)
      @named_arguments = normalize_named_arguments(named_arguments).freeze
      @aliases = Array(aliases).map { |name| name.to_s.dup.freeze }.freeze
    end

    def validate(filter_name, positional_count, supplied_arguments)
      unless positional_arguments.include?(positional_count)
        raise Liquid::FilterSignatureError, positional_argument_error(filter_name, positional_count)
      end

      supplied = supplied_arguments.transform_keys(&:to_s)
      unknown_arguments = supplied.keys - named_arguments.keys
      unless unknown_arguments.empty?
        raise Liquid::FilterSignatureError, "unknown keyword argument #{unknown_arguments.first.inspect} for filter #{filter_name.inspect}"
      end

      missing_arguments = named_arguments.select do |name, default|
        default.equal?(REQUIRED) && !supplied.key?(name)
      end.keys
      unless missing_arguments.empty?
        raise Liquid::FilterSignatureError, "missing required keyword argument #{missing_arguments.first.inspect} for filter #{filter_name.inspect}"
      end

      named_arguments.each do |name, default|
        supplied[name] = duplicate_default(default) unless default.equal?(REQUIRED) || supplied.key?(name)
      end

      supplied
    end

    private

    def normalize_positional_arguments(arguments)
      range = case arguments
      when nil
        0..INFINITY
      when Integer
        raise ::ArgumentError, "positional argument count cannot be negative" if arguments.negative?

        arguments..arguments
      when Range
        first = arguments.begin || 0
        last = arguments.end || INFINITY
        last -= 1 if arguments.exclude_end? && last != INFINITY
        first..last
      when Array
        raise ::ArgumentError, "positional argument range must contain minimum and maximum" unless arguments.length == 2

        first, last = arguments
        first ||= 0
        last ||= INFINITY
        first..last
      when Hash
        first = arguments[:minimum] || arguments[:min] || 0
        last = arguments[:maximum] || arguments[:max] || INFINITY
        first..last
      else
        raise ::ArgumentError, "positional arguments must be an integer or range"
      end

      raise ::ArgumentError, "minimum positional argument count cannot be negative" if range.begin.negative?
      raise ::ArgumentError, "minimum positional argument count exceeds maximum" if range.begin > range.end

      range
    end

    def normalize_named_arguments(arguments)
      case arguments
      when nil
        {}
      when Hash
        arguments.each_with_object({}) do |(name, default), normalized|
          normalized[name.to_s] = normalize_default(default)
        end
      when Array
        arguments.each_with_object({}) do |argument, normalized|
          case argument
          when Symbol, String
            normalized[argument.to_s] = REQUIRED
          when Array
            name, default = argument
            normalized[name.to_s] = normalize_default(default)
          when Hash
            argument.each do |name, default|
              normalized[name.to_s] = normalize_default(default)
            end
          else
            raise ::ArgumentError, "named arguments must be a hash or array"
          end
        end
      else
        raise ::ArgumentError, "named arguments must be a hash or array"
      end
    end

    def required_argument?(value)
      value.equal?(REQUIRED) || value == :required
    end

    def normalize_default(value)
      required_argument?(value) ? REQUIRED : value
    end

    def duplicate_default(value)
      immutable = value.is_a?(Numeric) || value.is_a?(Symbol) || value.is_a?(TrueClass) ||
        value.is_a?(FalseClass) || value.nil?
      return value if immutable || value.frozen?

      value.dup
    end

    def positional_argument_error(filter_name, positional_count)
      expected = if positional_arguments.begin == positional_arguments.end
        positional_arguments.begin.to_s
      elsif positional_arguments.end == INFINITY
        "at least #{positional_arguments.begin}"
      elsif positional_arguments.begin.zero?
        "at most #{positional_arguments.end}"
      else
        "#{positional_arguments.begin} to #{positional_arguments.end}"
      end

      "wrong number of positional arguments for filter #{filter_name.inspect} " \
        "(given #{positional_count}, expected #{expected})"
    end
  end
end
