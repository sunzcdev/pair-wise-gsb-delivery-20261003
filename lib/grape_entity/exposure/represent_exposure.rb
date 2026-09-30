# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      class RepresentExposure < Base
        attr_reader :using_class_name, :subexposure

        def setup(using_class_name, subexposure)
          @using_class = nil
          @using_class_name = using_class_name
          @subexposure = subexposure
        end

        def dup_args
          [*super, using_class_name, subexposure]
        end

        def ==(other)
          super &&
            @using_class_name == other.using_class_name &&
            @subexposure == other.subexposure
        end

        def value(entity, options)
          new_options = options.for_nesting(key(entity))
          objects = @subexposure.value(entity, options)

          if new_options[:partial_errors] && objects.respond_to?(:to_ary)
            represent_partial_collection(objects, new_options)
          else
            using_class.represent(objects, new_options)
          end
        end

        def valid?(entity)
          @subexposure.valid? entity
        end

        def using_class
          @using_class ||= if @using_class_name.respond_to? :constantize
                             @using_class_name.constantize
                           else
                             @using_class_name
                           end
        end

        private

        def represent_partial_collection(objects, options)
          strategy = @on_error || options[:on_error] || :null

          values = objects.to_ary.map.with_index do |object, index|
            options.with_attr_path(index) do
              begin
                using_class.unwrap_partial_value(using_class.new(object, options).presented)
              rescue StandardError => e
                options[:partial_errors] << PartialError.new((options[:attr_path] || []).dup, nil, e)
                strategy == :omit ? :omitted : nil
              end
            end
          end

          strategy == :omit ? values.reject { |value| value == :omitted } : values
        end

        def using_options_for(options)
          options.for_nesting(key)
        end
      end
    end
  end
end
