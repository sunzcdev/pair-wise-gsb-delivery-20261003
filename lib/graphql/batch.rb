require "graphql"
require "promise.rb"

module GraphQL
  module Batch
    BrokenPromiseError = ::Promise::BrokenError
    class NoExecutorError < StandardError; end

    class DependencyCycleError < StandardError
      attr_reader :loaders, :keys

      def self.for(requests)
        new(*requests)
      end

      class << self
        private

        def message_for(loaders, keys)
          chain = loaders.each_with_index.map do |loader, index|
            "#{loader_label(loader)} #{key_label(keys[index])}"
          end

          "Loader dependency cycle: #{chain.join(' -> ')}"
        end

        def loader_label(loader)
          klass = Object.instance_method(:class).bind_call(loader)
          name = Module.instance_method(:name).bind_call(klass) || "<AnonymousLoader>"
          "#{name}##{Object.instance_method(:object_id).bind_call(loader).to_s(16)}"
        end

        def key_label(key)
          klass = Object.instance_method(:class).bind_call(key)
          if CORE_INSPECTABLE_KEYS.include?(klass)
            inspect_method = Module.instance_method(:instance_method).bind_call(klass, :inspect)
            inspect_method.bind_call(key)
          else
            name = Module.instance_method(:name).bind_call(klass) || "<AnonymousKey>"
            "#{name}##{Object.instance_method(:object_id).bind_call(key).to_s(16)}"
          end
        end
      end

      def initialize(*requests)
        @loaders = requests.map(&:loader)
        @keys = requests.map(&:load_key)

        super(self.class.send(:message_for, @loaders, @keys))
      end

      CORE_INSPECTABLE_KEYS = [
        Symbol,
        Integer,
        Float,
        Rational,
        Complex,
        true.class,
        false.class,
        NilClass,
        String,
      ].freeze
      private_constant :CORE_INSPECTABLE_KEYS
    end

    def self.batch(executor_class: GraphQL::Batch::Executor)
      begin
        GraphQL::Batch::Executor.start_batch(executor_class)
        ::Promise.sync(yield)
      ensure
        GraphQL::Batch::Executor.end_batch
      end
    end

    def self.use(schema_defn, executor_class: GraphQL::Batch::Executor)
      if schema_defn.respond_to?(:trace_with)
        schema_defn.trace_with(GraphQL::Batch::SetupMultiplex::Trace, executor_class: executor_class)
      else
        instrumentation = GraphQL::Batch::SetupMultiplex.new(schema_defn, executor_class: executor_class)
        schema_defn.instrument(:multiplex, instrumentation)
      end

      if schema_defn.mutation
        require_relative "batch/mutation_field_extension"

        schema_defn.mutation.all_field_definitions.map do |field|
          field.extension(GraphQL::Batch::MutationFieldExtension)
        end
      end

      schema_defn.lazy_resolve(::Promise, :sync)
    end
  end
end

require_relative "batch/version"
require_relative "batch/loader"
require_relative "batch/executor"
require_relative "batch/setup_multiplex"
