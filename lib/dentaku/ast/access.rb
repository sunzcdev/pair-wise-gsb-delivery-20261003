require_relative "../exceptions"
require_relative "./node"
require_relative "../streaming"

module Dentaku
  module AST
    class Access < Node
      attr_reader :structure, :index

      def self.arity
        2
      end

      def self.min_param_count
        arity
      end

      def self.max_param_count
        arity
      end

      def initialize(data_structure, index)
        @structure = data_structure
        @index = index
      end

      def value(context = {})
        structure = @structure.value(context)
        index = @index.value(context)

        if Dentaku::Streaming.stream?(structure)
          return streamed_access(structure, index, context)
        end

        unless structure.respond_to?(:[]) && !structure.is_a?(::Numeric)
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: structure),
                "#{self.class} requires an indexable structure, but got #{structure.class}"
        end

        begin
          structure[index]
        rescue ::TypeError => e
          raise Dentaku::ArgumentError.for(:incompatible_type, actual: index), e.message
        end
      end

      def dependencies(context = {})
        @structure.dependencies(context) + @index.dependencies(context)
      end

      def type
        nil
      end

      def accept(visitor)
        visitor.visit_access(self)
      end

      private

      def streamed_access(structure, raw_index, context)
        session = Dentaku::Streaming.session(context)
        source_name = @structure.stream_source_name

        unless raw_index.is_a?(::Integer)
          raise Dentaku::ArgumentError.for(
            :incompatible_type, actual: raw_index, expected: Integer
          ), "streamed access requires an integer index, got #{raw_index.class}"
        end

        if session
          session.register(source_name, structure)
          idx = session.resolve_index(structure, raw_index, source_name,
                                      operation: "#{source_name}[]", location: location)
          unless session.valid_index?(idx, structure,
                                     operation: "#{source_name}[]", location: location)
            raise Dentaku::StreamIndexError.new(
              nil, index: raw_index,
              length: session.length(structure, source_name,
                                    operation: "#{source_name}[]", location: location),
              source: structure, operation: "#{source_name}[]",
              location: location
            )
          end

          session.fetch(structure, idx, source_name,
                        operation: "#{source_name}[]", location: location)
        else
          # No streaming session: never silently materialize.
          len = structure.stream_length
          idx = raw_index.negative? ? len + raw_index : raw_index
          raise Dentaku::StreamIndexError.new(
            nil, index: raw_index, length: len,
            source: structure, operation: "#{source_name}[]", location: location
          ) unless idx >= 0 && idx < len

          structure.read_range(idx, 1).first
        end
      end

      def compute_pure?
        @structure.pure? && @index.pure?
      end
    end
  end
end
