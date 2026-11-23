# frozen_string_literal: true

require "bigdecimal"

module ActiveRecord
  class PredicateBuilder
    class RangeHandler # :nodoc:
      RangeWithBinds = Struct.new(:begin, :end, :exclude_end?)

      def initialize(predicate_builder)
        @predicate_builder = predicate_builder
      end

      # Builds the predicate for a Range.
      #
      # When the column stores numbers on a fixed decimal grid, the bounds are
      # first interpreted on that grid: the lower bound moves up to the
      # smallest storable value inside the range, and the upper bound moves
      # down to the largest one. The predicate is then built inclusively, so it
      # selects exactly the storable values inside the range and never a value
      # outside it.
      #
      # An open bound (`nil` or an infinity) is left untouched, so it keeps its
      # SQL meaning: it does not constrain that side, and rows with a `NULL`
      # column are excluded from both the predicate and its negation.
      #
      # The same predicate is used by `where.not`, which only inverts it, so
      # both directions agree on the selected values.
      #
      # Raises ActiveRecord::RangePrecisionError when a bound cannot be handled
      # exactly, rather than truncating it into a different condition: either
      # the column does not report the grid it stores values on and the bound
      # is not a whole number, or the column type would store the narrowed
      # bound as a different value.
      def call(attribute, value)
        type = attribute.type_caster
        begin_value, end_value, exclude_end = bounds_on_column_grid(attribute, type, value)
        begin_bind = predicate_builder.build_bind_attribute(attribute, begin_value)
        end_bind = predicate_builder.build_bind_attribute(attribute, end_value)
        attribute.between(RangeWithBinds.new(begin_bind, end_bind, exclude_end))
      end

      private
        attr_reader :predicate_builder

        def bounds_on_column_grid(attribute, type, value)
          scale = stored_decimal_scale(type)

          if scale
            begin_value = begin_on_grid(value.begin, scale)
            end_value = end_on_grid(value.end, value.exclude_end?, scale)
            verify_storable!(attribute, type, begin_value, scale)
            verify_storable!(attribute, type, end_value, scale)

            # Both bounds now sit on the grid, so an excluded bound has already
            # been moved to the storable value just inside the range and the
            # predicate can be inclusive. An open bound keeps its flag, which
            # Arel does not use for it.
            exclude_end = value.exclude_end? && untyped_bound?(value.end)
            [begin_value, end_value, exclude_end]
          else
            if stored_decimal_scale_unknown?(type)
              verify_representable!(attribute, type, value.begin, value.end)
            end

            [value.begin, value.end, value.exclude_end?]
          end
        end

        def stored_decimal_scale(type)
          return unless type.respond_to?(:stored_decimal_scale)

          scale = type.stored_decimal_scale
          scale if scale.is_a?(::Integer) && scale >= 0
        end

        def stored_decimal_scale_unknown?(type)
          type.respond_to?(:stored_decimal_scale_known?) && !type.stored_decimal_scale_known?
        end

        # The smallest storable value at or above the bound. The lower bound of
        # a Range is always included, regardless of `exclude_end?`.
        def begin_on_grid(value, scale)
          rational = decimal_rational(value)
          return value if rational.nil?

          decimal_from_grid((rational / step(scale)).ceil * step(scale), scale)
        end

        # The largest storable value inside the bound. An excluded bound
        # moves down to the storable value strictly below it.
        def end_on_grid(value, exclude_end, scale)
          rational = decimal_rational(value)
          return value if rational.nil?

          steps = exclude_end ? (rational / step(scale)).ceil - 1 : (rational / step(scale)).floor
          decimal_from_grid(steps * step(scale), scale)
        end

        # The handler is shared between Ractors, so this must not memoize
        # into instance variables.
        def step(scale)
          Rational(1, 10**scale)
        end

        def decimal_from_grid(rational, scale)
          BigDecimal("#{(rational * 10**scale).to_i}e-#{scale}")
        end

        # Bounds that are not numbers at all (`nil`, infinities, strings the
        # column type cannot read) are left to the type and to SQL.
        def untyped_bound?(value)
          decimal_rational(value).nil?
        end

        def decimal_rational(value)
          case value
          when nil then nil
          when ::Integer then Rational(value, 1)
          when ::Rational then value
          when ::BigDecimal then value.to_r if value.finite?
          when ::Float then value.to_d.to_r if value.finite?
          when ::String
            decimal = BigDecimal(value, exception: false)
            decimal.to_r if decimal&.finite?
          end
        end

        # A grid-aligned bound must also survive the column type unchanged. A
        # type that rounds or truncates it would compare against a different
        # value, so report that instead of generating a different condition.
        def verify_storable!(attribute, type, value, scale)
          rational = decimal_rational(value)
          return if rational.nil?

          stored = decimal_rational(cast_through_type(type, value))
          return if stored.nil? || stored == rational

          stored_display = BigDecimal(stored.numerator) / BigDecimal(stored.denominator)
          raise RangePrecisionError,
            "Cannot express the Range bound #{value.to_s("F").inspect} for #{attribute.name.inspect}: " \
            "#{type.class.name} stores values on a scale of #{scale} but would store it as " \
            "#{stored_display}, so the condition would not match that bound. " \
            "Use bounds the column can store exactly."
        end

        def cast_through_type(type, value)
          type.cast(type.serialize(value))
        rescue ::StandardError
          nil
        end

        def verify_representable!(attribute, type, begin_value, end_value)
          [begin_value, end_value].each do |value|
            rational = decimal_rational(value)
            next if rational.nil? || rational.denominator == 1

            raise RangePrecisionError,
              "Cannot interpret the Range bound #{value.inspect} for #{attribute.name.inspect}: " \
              "#{type.class.name} does not report the decimal scale its values are stored at, " \
              "so the bound cannot be mapped onto the values the column can hold. " \
              "Declare a scale for the column, or use whole-number bounds."
          end
        end
    end
  end
end
