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
          using_class.represent(@subexposure.value(entity, options), new_options)
        end

        def partial_serializable_value(entity, options)
          return super unless options.partial?

          new_options = options.for_nesting(key(entity))
          value = @subexposure.valid_value(entity, options)

          if value.is_a?(Array)
            value.each_with_index.map do |item, index|
              new_options.with_collection_index(index) do
                using_class.represent_partial(item, new_options, options.partial_context).representation
              end
            end
          else
            using_class.represent_partial(value, new_options, options.partial_context).representation
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

        def using_options_for(options)
          options.for_nesting(key)
        end
      end
    end
  end
end
