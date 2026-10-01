# frozen_string_literal: true

module Grape
  class Entity
    module Exposure
      # An exposure that dispatches each object (or each element of a
      # collection) to the presenter selected by a {PolymorphicMapping},
      # based on the actual class of the object at representation time.
      class PolymorphicExposure < Base
        attr_reader :polymorphic_mapping, :subexposure

        def setup(polymorphic_mapping, subexposure)
          @polymorphic_mapping = polymorphic_mapping
          @subexposure = subexposure
        end

        def dup_args
          [*super, @polymorphic_mapping.dup, @subexposure]
        end

        def ==(other)
          super &&
            @polymorphic_mapping == other.polymorphic_mapping &&
            @subexposure == other.subexposure
        end

        def value(entity, options)
          new_options = options.for_nesting(key(entity))
          represent_value(@subexposure.value(entity, options), new_options)
        end

        def valid?(entity)
          @subexposure.valid?(entity)
        end

        private

        # Each element of a collection is dispatched individually.
        def represent_value(value, options)
          if value.respond_to?(:to_ary)
            value.to_ary.map { |item| represent_one(item, options) }
          else
            represent_one(value, options)
          end
        end

        # +nil+ has a fixed behavior: it is never dispatched and
        # always represents as +nil+.
        def represent_one(object, options)
          return nil if object.nil?

          @polymorphic_mapping.resolve(object).represent(object, options)
        end
      end
    end
  end
end
