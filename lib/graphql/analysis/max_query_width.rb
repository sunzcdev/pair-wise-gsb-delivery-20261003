# frozen_string_literal: true
module GraphQL
  module Analysis
    class MaxQueryWidth < QueryWidth
      def result
        configured_max_width = if query
          query.max_width
        else
          multiplex.schema.max_width
        end

        if configured_max_width && @max_width > configured_max_width
          path = @max_width_path || []
          location = @max_width_type.graphql_name
          location += " at `#{path.join(".")}`" unless path.empty?
          GraphQL::AnalysisError.new(
            "Query has response width of #{@max_width} on #{location}, which exceeds max width of #{configured_max_width}"
          )
        else
          nil
        end
      end
    end
  end
end
