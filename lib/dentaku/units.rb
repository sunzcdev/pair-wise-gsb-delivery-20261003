require 'dentaku/exceptions'
require 'dentaku/numeric_parser'
require 'dentaku/print_visitor'

module Dentaku
  # Optional unit-dimension mode. A Quantity couples a numeric magnitude with
  # a Unit (a dimension signature plus a conversion factor to the dimension's
  # base unit), so arithmetic can propagate dimensions, convert compatible
  # units automatically, and reject dimensionally incompatible operations.
  module Units
    # reserved context key (like __evaluation_mode) marking that the current
    # evaluation runs in units mode; enables dimension checks that need the
    # whole expression, such as conditional-branch consistency
    MODE_KEY = "__units_mode".freeze

    class Error < BaseError; end

    class UnknownUnitError < Error
      def initialize(name)
        super("unknown unit: #{name.inspect}")
      end
    end

    class ModeError < Error; end

    # raised when an operation combines quantities whose dimensions cannot be
    # reconciled; carries the printed form of the innermost subexpression
    # where the mismatch surfaced
    class MismatchError < Error
      attr_reader :operation, :left, :right, :location

      def initialize(operation, left, right)
        @operation = operation
        @left = left.is_a?(Quantity) ? left : Quantity.dimensionless(left)
        @right = right.is_a?(Quantity) ? right : Quantity.dimensionless(right)
        super(build_message)
      end

      # records the subexpression that failed; the first (innermost) location
      # wins, so the error points at the exact offending operation
      def locate(node)
        @location ||= node && Dentaku::PrintVisitor.new(node).to_s
        self
      end

      def message
        @location ? "#{super} in subexpression `#{@location}`" : super
      end

      private

      def build_message
        "cannot #{operation} #{left} and #{right}: " \
          "incompatible dimensions (#{left.unit.describe} vs #{right.unit.describe})"
      end
    end

    # a dimension signature (e.g. {length: 1, time: -1}) plus the factor that
    # converts a magnitude in this unit to the base unit of each dimension
    class Unit
      attr_reader :dimensions, :factor, :label

      def initialize(dimensions = {}, factor = 1.0, label = nil)
        @dimensions = dimensions.reject { |_, exponent| exponent.zero? }.freeze
        @factor = factor
        @label = label || self.class.derive_label(@dimensions)
      end

      def dimensionless?
        dimensions.empty?
      end

      def compatible?(other)
        other.is_a?(Unit) && dimensions == other.dimensions
      end

      def *(other)
        Unit.new(
          merge_dimensions(other, :+),
          factor * other.factor,
          self.class.combine_label(label, other.label, "*")
        )
      end

      def /(other)
        Unit.new(
          merge_dimensions(other, :-),
          factor.quo(other.factor),
          quotient_label(other)
        )
      end

      def **(exponent)
        Unit.new(
          dimensions.transform_values { |e| e * exponent },
          factor**exponent,
          exponent == 1 ? label : "#{label}^#{exponent}"
        )
      end

      # human-readable dimension signature, e.g. "length", "length/time"
      def describe
        return "dimensionless" if dimensionless?

        positive = dimensions.select { |_, e| e.positive? }.map { |d, e| format_dimension(d, e) }
        negative = dimensions.select { |_, e| e.negative? }.map { |d, e| format_dimension(d, -e) }
        numerator = positive.empty? ? "1" : positive.join("*")
        negative.empty? ? numerator : "#{numerator}/#{negative.join('*')}"
      end

      def to_s
        label
      end

      def self.combine_label(left_label, right_label, separator)
        return right_label if left_label.empty?
        return left_label if right_label.empty?

        "#{left_label}#{separator}#{right_label}"
      end

      private

      def quotient_label(other)
        return "" if label.empty? && other.label.empty?
        return "1/#{other.label}" if label.empty?

        self.class.combine_label(label, other.label, "/")
      end

      def merge_dimensions(other, operation)
        merged = dimensions.dup
        other.dimensions.each do |dimension, exponent|
          merged[dimension] = (merged[dimension] || 0).public_send(operation, exponent)
        end
        merged.reject { |_, e| e.zero? }
      end

      def format_dimension(name, exponent)
        exponent == 1 ? name.to_s : "#{name}^#{exponent}"
      end

      def self.derive_label(dimensions)
        return "" if dimensions.empty?

        dimensions.map { |d, e| e == 1 ? d.to_s : "#{d}^#{e}" }.join("*")
      end

      DIMENSIONLESS = new({}, 1.0, "")
    end

    # a numeric magnitude tagged with a Unit
    class Quantity
      include Comparable

      attr_reader :value, :unit

      def initialize(value, unit)
        @value = value
        @unit = unit.is_a?(Unit) ? unit : Units.fetch(unit)
      end

      def self.dimensionless(value)
        new(value, Unit::DIMENSIONLESS)
      end

      def dimensionless?
        unit.dimensionless?
      end

      def compatible?(other)
        other.is_a?(Quantity) && unit.compatible?(other.unit)
      end

      # magnitude expressed in the dimension's base unit (e.g. ms -> s)
      def base_value
        value * unit.factor
      end

      # convert to a compatible unit, raising MismatchError otherwise
      def to(target)
        target = Units.fetch(target) unless target.is_a?(Unit)
        unless unit.compatible?(target)
          raise MismatchError.new(:convert, self, Quantity.new(0, target))
        end

        Quantity.new(value * unit.factor.quo(target.factor), target)
      end

      def +(other)
        additive(other, :+, :add)
      end

      def -(other)
        additive(other, :-, :subtract)
      end

      def *(other)
        case other
        when Quantity then combine(other, :*)
        when ::Numeric then Quantity.new(value * other, unit)
        else incompatible_operand!(other)
        end
      end

      def /(other)
        case other
        when Quantity then combine(other, :/)
        when ::Numeric then Quantity.new(value / other, unit)
        else incompatible_operand!(other)
        end
      end

      def %(other)
        case other
        when Quantity
          raise MismatchError.new(:modulo, self, other) unless compatible?(other)

          Quantity.new(value % other.to(unit).value, unit)
        when ::Numeric
          Quantity.new(value % other, unit)
        else
          incompatible_operand!(other)
        end
      end

      def **(other)
        exponent = exponent_value(other)
        new_unit = unit**exponent
        new_value = value**exponent
        return new_value * new_unit.factor if new_unit.dimensionless?

        Quantity.new(new_value, new_unit)
      end

      def <=>(other)
        other = coerce_operand(other)
        raise MismatchError.new(:compare, self, other) unless compatible?(other)

        value <=> other.to(unit).value
      end

      def ==(other)
        return false unless other.is_a?(Quantity) || other.is_a?(::Numeric)

        other = coerce_operand(other)
        raise MismatchError.new(:compare, self, other) unless compatible?(other)

        value == other.to(unit).value
      end

      # plain numerics lift to dimensionless quantities so that e.g.
      # `100 / speed` and `2 * duration` work through Ruby's coerce protocol
      def coerce(numeric)
        [Quantity.dimensionless(numeric), self]
      end

      def zero?
        value.zero?
      end

      def abs
        Quantity.new(value.abs, unit)
      end

      def round(places = 0)
        Quantity.new(value.round(places), unit)
      end

      def roundup(places = 0)
        tens = 10.0**places
        result = (value * tens).ceil / tens
        Quantity.new(places <= 0 ? result.to_i : result, unit)
      end

      def rounddown(places = 0)
        tens = 10.0**places
        result = (value * tens).floor / tens
        Quantity.new(places <= 0 ? result.to_i : result, unit)
      end

      def to_s
        dimensionless? ? value.to_s : "#{value} #{unit.label}"
      end

      def inspect
        "#<Dentaku::Units::Quantity #{self}>"
      end

      private

      def additive(other, operator, operation)
        other = Quantity.dimensionless(other) if other.is_a?(::Numeric)
        incompatible_operand!(other) unless other.is_a?(Quantity)

        unless compatible?(other)
          # zero is the additive identity in any dimension, so `0 + q` and
          # `q - 0` stay legal (this also keeps SUM's 0-seeded reduce working)
          return other if dimensionless? && zero? && operator == :+
          return Quantity.new(-other.value, other.unit) if dimensionless? && zero?
          return self if other.dimensionless? && other.zero?

          raise MismatchError.new(operation, self, other)
        end

        Quantity.new(value.public_send(operator, other.to(unit).value), unit)
      end

      def combine(other, operator)
        new_unit = unit.public_send(operator, other.unit)
        new_value = value.public_send(operator, other.value)
        # dimensions that cancel out (e.g. m / m) yield a plain
        # dimensionless ratio rather than a quantity
        return new_value * new_unit.factor if new_unit.dimensionless?

        Quantity.new(new_value, new_unit)
      end

      def exponent_value(other)
        case other
        when Quantity
          raise MismatchError.new(:exponentiate, self, other) unless other.dimensionless?

          other.base_value
        when ::Numeric
          other
        else
          incompatible_operand!(other)
        end
      end

      def coerce_operand(other)
        case other
        when Quantity then other
        when ::Numeric then Quantity.dimensionless(other)
        else incompatible_operand!(other)
        end
      end

      def incompatible_operand!(other)
        raise Dentaku::ArgumentError.for(:incompatible_type, actual: other, expected: ::Numeric)
      end
    end

    @units = {}

    class << self
      # convenience constructor: Dentaku::Units.quantity(500, :ms)
      def quantity(value, unit)
        Quantity.new(value, unit)
      end

      def register(name, dimensions, factor)
        @units[name.to_sym] = Unit.new(dimensions, factor, name.to_s)
      end

      def fetch(name)
        @units.fetch(name.to_sym) { raise UnknownUnitError, name }
      end

      def registered?(name)
        @units.key?(name.to_sym)
      end

      # like NumericParser.ensure_numeric!, but passes quantities through so
      # unit-aware functions (SUM, MAX, ...) can propagate dimensions
      def unwrap_numeric(value)
        value.is_a?(Quantity) ? value : Dentaku::NumericParser.ensure_numeric!(value)
      end

      # used by conditional nodes: both branch results must agree on dimension
      def assert_compatible!(a, b, node: nil)
        a_quantity = a.is_a?(Quantity)
        b_quantity = b.is_a?(Quantity)
        compatible = a_quantity && b_quantity ? a.compatible?(b) : a_quantity == b_quantity
        return if compatible

        raise MismatchError.new(:return, a, b).locate(node)
      end

      # used by custom functions registered with `unit:`: plain numerics are
      # tagged with the declared unit, quantities must be convertible to it
      def coerce_result(value, unit, node: nil)
        unit = fetch(unit) unless unit.is_a?(Unit)

        case value
        when Quantity
          value.to(unit)
        when ::Numeric
          Quantity.new(value, unit)
        else
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: value, expected: ::Numeric)
        end
      rescue MismatchError => e
        raise e.locate(node)
      end
    end

    # time
    register :ms, { time: 1 }, 0.001
    register :s,  { time: 1 }, 1
    register :min, { time: 1 }, 60
    register :h,  { time: 1 }, 3600
    register :hr, { time: 1 }, 3600
    # length
    register :mm, { length: 1 }, 0.001
    register :cm, { length: 1 }, 0.01
    register :m,  { length: 1 }, 1
    register :km, { length: 1 }, 1000
    register :ft, { length: 1 }, 0.3048
    register :mi, { length: 1 }, 1609.344
    # mass
    register :g,  { mass: 1 }, 0.001
    register :kg, { mass: 1 }, 1
  end
end
