# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class FormatterExposure < Base
        attr_reader :format_with

        def setup(format_with)
          @format_with = format_with
        end

        def dup_args
          [*super, format_with]
        end

        def ==(other)
          super && @format_with == other.format_with
        end

        def value(entity, _options)
          formatters = entity.class.formatters
          if formatters[@format_with]
            entity.exec_with_attribute(attribute, reread: reread?, &formatters[@format_with])
          else
            attribute_value = entity.exec_with_attribute(attribute, reread: reread?)
            entity.send(@format_with, attribute_value)
          end
        end
      end
    end
  end
end
