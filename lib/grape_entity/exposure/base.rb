# frozen_string_literal: true

require 'active_support'
require 'active_support/core_ext'

module Grape
  class Entity
    module Exposure
      class Base
        attr_reader :attribute, :is_safe, :documentation, :override, :conditions, :for_merge

        def self.new(attribute, options, conditions, ...)
          super(attribute, options, conditions).tap { |e| e.setup(...) }
        end

        def initialize(attribute, options, conditions)
          @attribute = attribute.try(:to_sym)
          @options = options
          key = options[:as] || attribute
          @key = key.respond_to?(:to_sym) ? key.to_sym : key
          @is_safe = options[:safe]
          @default_value = options[:default]
          @for_merge = options[:merge]
          @attr_path_proc = options[:attr_path]
          @documentation = options[:documentation]
          @override = options[:override]
          @conditions = conditions
        end

        def dup(&block)
          self.class.new(*dup_args, &block)
        end

        def dup_args
          [@attribute, @options, @conditions.map(&:dup)]
        end

        def ==(other)
          self.class == other.class &&
            @attribute == other.attribute &&
            @options == other.options &&
            @conditions == other.conditions
        end

        def setup; end

        def nesting?
          false
        end

        # if we have any nesting exposures with the same name.
        def deep_complex_nesting?(entity) # rubocop:disable Lint/UnusedMethodArgument
          false
        end

        def valid?(entity)
          is_delegatable = entity.delegator.delegatable?(@attribute) || entity.respond_to?(@attribute, true)
          if @is_safe
            is_delegatable
          else
            is_delegatable || raise(
              NoMethodError,
              "#{entity.class.name} missing attribute `#{@attribute}' on #{entity.object}"
            )
          end
        end

        def value(entity, options)
          value_from_raw(evaluate_raw_value(entity, options), entity, options)
        end

        def value_from_raw(raw_value, _entity, _options)
          raw_value
        end

        def evaluate_raw_value(entity, _options)
          entity.delegate_attribute(attribute)
        end

        def serializable_value(entity, options)
          partial_output = valid_value(entity, options)

          if partial_output.respond_to?(:serializable_hash)
            partial_output.serializable_hash
          elsif partial_output.is_a?(Array) && partial_output.all? { |o| o.respond_to?(:serializable_hash) }
            partial_output.map(&:serializable_hash)
          elsif partial_output.is_a?(Hash)
            partial_output.each do |key, value|
              partial_output[key] = value.serializable_hash if value.respond_to?(:serializable_hash)
            end
          else
            partial_output
          end
        end

        def valid_value(entity, options)
          return unless valid?(entity)

          output = evaluated_value(entity, options)
          output.blank? && !@default_value.nil? ? @default_value : output
        end

        def conditions_met?(entity, options)
          context = evaluation_context_for(options)

          if context
            context.evaluating(self) do
              @conditions.all? { |condition| condition.met? entity, options }
            end
          else
            @conditions.all? { |condition| condition.met? entity, options }
          end
        end

        def evaluated_value(entity, options)
          context = evaluation_context_for(options)
          context ? context.value(self, entity, options) : value(entity, options)
        end

        def evaluated_raw_value(entity, options)
          context = evaluation_context_for(options)
          context ? context.raw_value(self, entity, options) : evaluate_raw_value(entity, options)
        end

        def condition_raw_value(entity, options)
          evaluated_raw_value(entity, options)
        end

        def should_return_key?(options)
          options.should_return_key?(@key)
        end

        def conditional?
          !@conditions.empty?
        end

        def should_expose?(entity, options)
          should_return_key?(options) && conditions_met?(entity, options)
        end

        def attr_path(entity, options)
          if @attr_path_proc
            entity.exec_with_object(options, &@attr_path_proc)
          else
            @key
          end
        end

        def key(entity = nil)
          @key.respond_to?(:call) ? entity.exec_with_object(@options, &@key) : @key
        end

        def with_attr_path(entity, options, &block)
          path_part = attr_path(entity, options)
          options.with_attr_path(path_part, &block)
        end

        def override?
          @override
        end

        def evaluation_context_for(options)
          options.evaluation_context if options.respond_to?(:evaluation_context)
        end

        protected

        attr_reader :options
      end
    end
  end
end
