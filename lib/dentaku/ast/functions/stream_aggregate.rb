require_relative '../function'

module Dentaku
  module AST
    module Functions
      # Shared streaming machinery for aggregate functions that can consume a
      # length-aware, range-readable input (see Dentaku::Streaming) without
      # ever materializing the whole sequence.
      module StreamAggregate
        def self.included(base)
          base.singleton_class.send(:define_method, :allows_empty_call?) { true }
        end

        def supports_streaming?
          true
        end

        def value(context = {})
          validate_arg_count!

          session = Dentaku::Streaming.session(context)

          if session
            stream_index = @args.index do |arg|
              Dentaku::Streaming.stream?(arg.value(context))
            end

            if stream_index
              return aggregate_stream(
                session,
                @args[stream_index].value(context),
                @args[stream_index],
                context
              )
            end
          end

          aggregate_arrays(@args.map { |arg| arg.value(context) })
        end

        private

        # Default non-streaming behavior, overridden by each function.
        def aggregate_arrays(values)
          raise NotImplementedError
        end

        def aggregate_stream(session, stream, stream_node, context)
          raise NotImplementedError
        end

        def each_numeric(session, stream, node_name, &block)
          unless block
            return Enumerator.new do |yielder|
              each_numeric(session, stream, node_name) { |sample| yielder << sample }
            end
          end

          session.each(stream, node_name,
                      operation: function_name, location: location) do |sample|
            block.call(Dentaku::NumericParser.ensure_numeric!(sample))
          end
        end

        def stream_name(stream_node)
          stream_node.stream_source_name || stream_node.to_s
        end
      end
    end
  end
end
