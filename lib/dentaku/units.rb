require 'bigdecimal'
require 'dentaku/exceptions'
require 'dentaku/numeric_parser'
require 'dentaku/print_visitor'

module Dentaku
  # Raised when quantities with incompatible dimensions are combined
  # (addition, subtraction, comparison, modulo) or when conditional
  # branches disagree on their result dimension. Carries the offending
  # subexpression so the error can be located inside a larger formula.
  class UnitError < BaseError
    attr_reader :operation, :left_dimension, :right_dimension, :expression

    def initialize(operation: nil, left: nil, right: nil, expression: nil, message: nil)
      @operation = operation
      @left_dimension = left
      @right_dimension = right
      @expression = expression
      @message = message
      super(build_message)
    end

    # returns a copy annotated with the subexpression that failed; the
    # innermost annotation wins so the error points at the smallest
    # offending subexpression
    def with_expression(expression)
      return self if @expression

      self.class.new(
        operation: operation,
        left: left_dimension,
        right: right_dimension,
        expression: expression,
        message: @message
      )
    end

    private

    def build_message
      base = @message || default_message
      @expression ? %(#{base} in "#{@expression}") : base
    end

    def default_message
      case operation
      when :add, :subtract, :compare, :modulo
        "cannot #{operation} quantities with incompatible dimensions (#{left_dimension} vs #{right_dimension})"
      when :branch
        "conditional branches have incompatible dimensions (#{left_dimension} vs #{right_dimension})"
      else
        "incompatible dimensions (#{left_dimension} vs #{right_dimension})"
      end
    end
  end

  module Units
    # reserved context key (like __evaluation_mode) that enables
    # dimension checks which require evaluating untaken branches
    CONTEXT_KEY = "__units_mode".freeze

    # unit => [dimension, factor to the dimension's base unit]
    DEFAULT_UNITS = {
      s:   [:time, BigDecimal("1")],
      ms:  [:time, BigDecimal("0.001")],
      min: [:time, BigDecimal("60")],
      h:   [:time, BigDecimal("3600")],
      m:   [:length, BigDecimal("1")],
      km:  [:length, BigDecimal("1000")],
      cm:  [:length, BigDecimal("0.01")],
      mm:  [:length, BigDecimal("0.001")],
      kg:  [:mass, BigDecimal("1")],
      g:   [:mass, BigDecimal("0.001")]
    }.freeze

    class << self
      def registry
        @registry ||= DEFAULT_UNITS.dup
      end

      # register a custom unit, e.g. Units.register(:ft, :length, 0.3048)
      def register(unit, dimension, factor_to_base)
        registry[unit.to_sym] = [dimension.to_sym, BigDecimal(factor_to_base.to_s)]
      end

      def quantity(value, unit = nil)
        Quantity.new(value, unit)
      end

      def quantity?(value)
        value.is_a?(Quantity)
      end

      # like NumericParser.ensure_numeric!, but passes quantities through
      # so unit-aware functions can propagate dimensions
      def numeric_or_quantity(value)
        quantity?(value) ? value : Dentaku::NumericParser.ensure_numeric!(value)
      end

      def coerce(value)
        quantity?(value) ? value : Quantity.new(value)
      end

      # normalize a unit argument (nil, symbol, or {unit => exponent} hash)
      # into a {unit => exponent} hash
      def normalize(unit)
        case unit
        when nil
          {}
        when Hash
          unit.each_with_object({}) do |(name, exponent), normalized|
            name = name.to_sym
            validate_unit!(name)
            exponent = Integer(exponent)
            normalized[name] = exponent unless exponent.zero?
          end
        else
          name = unit.to_sym
          validate_unit!(name)
          { name => 1 }
        end
      end

      def dimension_of(units)
        units.each_with_object(Hash.new(0)) do |(name, exponent), dimension|
          dimension[registry.fetch(name).first] += exponent
        end.reject { |_, exponent| exponent.zero? }
      end

      def factor_product(units)
        units.reduce(BigDecimal("1")) do |product, (name, exponent)|
          product * registry.fetch(name).last**exponent
        end
      end

      def describe(dimension)
        return "dimensionless" if dimension.empty?

        format_units(dimension)
      end

      def format_units(units)
        format = ->(name, exponent) { exponent.abs == 1 ? name.to_s : "#{name}^#{exponent.abs}" }
        numerator = units.select { |_, e| e > 0 }.map { |u, e| format.call(u, e) }.join("*")
        denominator = units.select { |_, e| e < 0 }.map { |u, e| format.call(u, e) }.join("*")
        numerator = "1" if numerator.empty?
        denominator.empty? ? numerator : "#{numerator}/#{denominator}"
      end

      # recursively convert {value:, unit:} hashes in input data to
      # quantities; anything else passes through untouched
      def coerce_data(data)
        return data unless data.is_a?(Hash)

        if quantity_spec?(data)
          value = data.key?(:value) ? data[:value] : data["value"]
          unit = data[:unit] || data["unit"] || data[:units] || data["units"]
          return Quantity.new(value, unit)
        end

        data.each_with_object({}) { |(k, v), coerced| coerced[k] = coerce_data(v) }
      end

      # wrap a plain numeric in its declared unit (per the calculator's
      # units: {name => unit} declarations)
      def apply_declared(value, unit)
        return value if unit.nil? || value.is_a?(Quantity)
        return value unless value.is_a?(::Numeric)

        Quantity.new(value, unit)
      end

      # used by conditionals: both branch results must agree on dimension
      def assert_compatible!(left, right, node: nil)
        left_dimension = left.is_a?(Quantity) ? left.dimension : {}
        right_dimension = right.is_a?(Quantity) ? right.dimension : {}
        return if left_dimension == right_dimension

        raise UnitError.new(
          operation: :branch,
          left: describe(left_dimension),
          right: describe(right_dimension),
          expression: node && Dentaku::PrintVisitor.new(node).to_s
        )
      end

      # annotate a UnitError with the subexpression that raised it
      def locate(error, node)
        error.with_expression(Dentaku::PrintVisitor.new(node).to_s)
      end

      private

      def quantity_spec?(hash)
        (hash.key?(:value) || hash.key?("value")) &&
          (hash.key?(:unit) || hash.key?("unit") || hash.key?(:units) || hash.key?("units"))
      end

      def validate_unit!(name)
        return if registry.key?(name)

        raise ::ArgumentError, "unknown unit: #{name}"
      end
    end

    # A numeric value tagged with units, e.g. Quantity.new(500, :ms).
    # Arithmetic propagates and combines dimensions; addition,
    # subtraction, comparison and modulo require matching dimensions and
    # convert between compatible units automatically. Operations whose
    # dimensions cancel out (e.g. m / m) return a plain numeric.
    class Quantity
      include Comparable

      attr_reader :value, :units

      def initialize(value, unit = nil)
        @value = value
        @units = Units.normalize(unit)
      end

      def dimension
        @dimension ||= Units.dimension_of(units)
      end

      def dimensionless?
        dimension.empty?
      end

      # value expressed in the dimension's base units
      def base_value
        @base_value ||= value * Units.factor_product(units)
      end

      # value re-expressed in the given units (must be the same dimension)
      def in_units(target_units)
        return value if target_units == units

        base_value / Units.factor_product(target_units)
      end

      def +(other)
        other = Units.coerce(other)
        assert_compatible!(other, :add)
        build(value + other.in_units(units), units)
      end

      def -(other)
        other = Units.coerce(other)
        assert_compatible!(other, :subtract)
        build(value - other.in_units(units), units)
      end

      def *(other)
        other = Units.coerce(other)
        build(value * other.value, merge_units(other.units, 1))
      end

      def /(other)
        other = Units.coerce(other)
        raise Dentaku::ZeroDivisionError if other.value.respond_to?(:zero?) && other.value.zero?

        build(decimal(value) / decimal(other.value), merge_units(other.units, -1))
      end

      def %(other)
        other = Units.coerce(other)
        assert_compatible!(other, :modulo)
        raise Dentaku::ZeroDivisionError if other.value.respond_to?(:zero?) && other.value.zero?

        build(decimal(value) % decimal(other.in_units(units)), units)
      end

      def **(other)
        other = Units.coerce(other)
        unless other.dimensionless?
          raise UnitError.new(
            message: "exponent must be dimensionless, got #{Units.describe(other.dimension)}"
          )
        end

        exponent = other.value
        return build(value**exponent, {}) if dimensionless?

        unless exponent.is_a?(Integer) || exponent == exponent.to_i
          raise UnitError.new(
            message: "cannot raise a quantity with dimension (#{Units.describe(dimension)}) " \
                     "to the non-integer power #{exponent}"
          )
        end

        build(value**exponent, scale_units(exponent.to_i))
      end

      def <=>(other)
        other = Units.coerce(other)
        assert_compatible!(other, :compare)
        base_value <=> other.base_value
      end

      # defined explicitly (rather than via Comparable) so that comparing
      # incompatible dimensions raises instead of silently returning false
      def ==(other)
        return false unless other.is_a?(Quantity) || other.is_a?(::Numeric)

        (self <=> other).zero?
      end

      def coerce(numeric)
        [Quantity.new(numeric), self]
      end

      def -@
        Quantity.new(-value, units)
      end

      def abs
        Quantity.new(value.abs, units)
      end

      def round(ndigits = 0)
        Quantity.new(value.round(ndigits), units)
      end

      def ceil(ndigits = 0)
        Quantity.new(value.ceil(ndigits), units)
      end

      def floor(ndigits = 0)
        Quantity.new(value.floor(ndigits), units)
      end

      def zero?
        value.respond_to?(:zero?) && value.zero?
      end

      def to_f
        value.to_f
      end

      def to_i
        value.to_i
      end

      def to_s
        rendered = value.is_a?(BigDecimal) ? value.to_s("F") : value.to_s
        units.empty? ? rendered : "#{rendered} #{Units.format_units(units)}"
      end

      def inspect
        "#<Dentaku::Units::Quantity #{self}>"
      end

      private

      def assert_compatible!(other, operation)
        return if dimension == other.dimension

        raise UnitError.new(
          operation: operation,
          left: Units.describe(dimension),
          right: Units.describe(other.dimension)
        )
      end

      # operations whose dimensions cancel out return a plain numeric
      def build(result_value, result_units)
        result_units.empty? ? result_value : Quantity.new(result_value, result_units)
      end

      def merge_units(other_units, sign)
        merged = units.dup
        other_units.each { |name, exponent| merged[name] = (merged[name] || 0) + sign * exponent }
        merged.reject { |_, exponent| exponent.zero? }
      end

      def scale_units(factor)
        units.each_with_object({}) do |(name, exponent), scaled|
          scaled[name] = exponent * factor
        end.reject { |_, exponent| exponent.zero? }
      end

      def decimal(number)
        BigDecimal(number.to_s, Float::DIG + 1)
      rescue ::ArgumentError, ::TypeError
        number
      end
    end
  end
end
