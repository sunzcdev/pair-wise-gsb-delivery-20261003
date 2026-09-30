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
            entity.exec_with_attribute(attribute, &formatters[@format_with])
          else
            entity.send(@format_with, entity.delegate_attribute(attribute))
          end
        end

        def valid_value(entity, options)
          return super unless options.partial? && @options[:on_format_error]
          return unless valid?(entity)

          value = entity.delegate_attribute(attribute)

          begin
            output = format_value(entity, value)
          rescue StandardError => exception
            failure = handle_partial_format_error(entity, options, exception)
            return failure if @options[:on_format_error] == :omit

            return nil
          end

          output.blank? && !@default_value.nil? ? @default_value : output
        end

        private

        def format_value(entity, value)
          formatter = entity.class.formatters[@format_with]
          if formatter
            entity.instance_exec(value, &formatter)
          else
            entity.send(@format_with, value)
          end
        end
      end
    end
  end
end
