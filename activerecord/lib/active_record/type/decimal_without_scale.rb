# frozen_string_literal: true

module ActiveRecord
  module Type
    class DecimalWithoutScale < ActiveModel::Type::BigInteger # :nodoc:
      def type
        :decimal
      end

      # Adapters use this type for decimal columns the database reports as
      # having no decimals, so values are cast to whole numbers both on write
      # and on read and the stored grid is the integers.
      def stored_decimal_scale
        0
      end

      def type_cast_for_schema(value)
        value.to_s.inspect
      end
    end
  end
end
