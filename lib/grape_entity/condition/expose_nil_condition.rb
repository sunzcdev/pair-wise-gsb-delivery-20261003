# frozen_string_literal: true

module Grape
  class Entity
    module Condition
      class ExposeNilCondition < Condition::Base
        attr_reader :block

        def setup(attribute, has_proc, source_block)
          @attribute = attribute
          @has_proc = has_proc
          @source_block = source_block
          @block = ->(entity, options = Options.new) { if_value(entity, options) }
        end

        def if_value(entity, options)
          context = options.evaluation_context if options.respond_to?(:evaluation_context)
          value = context ? context.condition_value(entity, options) : evaluate_without_context(entity, options)
          value.nil?
        end

        private

        def evaluate_without_context(entity, options)
          return entity.exec_with_object(options, &@source_block) if @has_proc

          object = entity.is_a?(Grape::Entity) ? entity.object : entity
          delegator = Delegator.new(object)
          if entity.is_a?(Grape::Entity) && delegator.accepts_options?
            delegator.delegate(@attribute, **entity.class.delegation_opts)
          else
            delegator.delegate(@attribute)
          end
        end
      end
    end
  end
end
