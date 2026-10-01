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

        def value_from_raw(raw_value, entity, _options)
          formatters = entity.class.formatters
          if formatters[@format_with]
            entity.instance_exec(raw_value, &formatters[@format_with])
          else
            entity.send(@format_with, raw_value)
          end
        end
      end
    end
  end
end
