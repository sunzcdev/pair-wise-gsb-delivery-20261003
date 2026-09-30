# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class FormatterBlockExposure < Base
        attr_reader :format_with

        def setup(&format_with)
          @format_with = format_with
        end

        def dup
          super(&@format_with)
        end

        def ==(other)
          super && @format_with == other.format_with
        end

        def value(entity, _options)
          entity.exec_with_attribute(attribute, &@format_with)
        end

        def valid_value(entity, options)
          return super unless options.partial? && @options[:on_format_error]
          return unless valid?(entity)

          value = entity.delegate_attribute(attribute)

          begin
            output = entity.instance_exec(value, &@format_with)
          rescue StandardError => exception
            failure = handle_partial_format_error(entity, options, exception)
            return failure if @options[:on_format_error] == :omit

            return nil
          end

          output.blank? && !@default_value.nil? ? @default_value : output
        end
      end
    end
  end
end
