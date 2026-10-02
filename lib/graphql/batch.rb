require "graphql"
require "promise.rb"

module GraphQL
  module Batch
    BrokenPromiseError = ::Promise::BrokenError
    class NoExecutorError < StandardError; end

    def self.batch(executor_class: GraphQL::Batch::Executor, isolated: false)
      begin
        if isolated
          GraphQL::Batch::Executor.start_isolated_batch(executor_class)
          result = ::Promise.sync(yield)
          # Settle any remaining loads inside the isolated scope before restoring
          # the parent executor, so pending inner promises can't depend on a
          # scope that has already exited.
          GraphQL::Batch::Executor.current.wait_all
          result
        else
          GraphQL::Batch::Executor.start_batch(executor_class)
          ::Promise.sync(yield)
        end
      ensure
        if isolated
          GraphQL::Batch::Executor.end_isolated_batch
        else
          GraphQL::Batch::Executor.end_batch
        end
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
