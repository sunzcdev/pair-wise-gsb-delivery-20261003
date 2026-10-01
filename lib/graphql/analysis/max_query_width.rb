# frozen_string_literal: true
module GraphQL
  module Analysis
    # Used under the hood to implement response-key width validation,
    # see {Schema#max_width} and {Query#max_width}
    #
    # Counting the number of field nodes in an AST overestimates the width
    # because the same response key can be selected many times (via repeated
    # fragments or merged selections) while only ever occupying one key in the
    # response. This analyzer counts the number of distinct response keys that
    # may be present in a single response object at the same time, so mutually
    # exclusive type branches only contribute their widest reachable possibility
    # and fields skipped with known variables are ignored.
    class MaxQueryWidth < QueryWidth
      def result
        configured_max_width = if query
          query.max_width
        else
          multiplex.schema.max_width
        end

        return nil if configured_max_width.nil?

        # Report the widest reachable violation (for abstract types several
        # mutually exclusive object types may exceed the limit).
        violation = width_violations(configured_max_width).max_by { |v| v[:width] }
        return nil if violation.nil?

        path = violation[:path]
        type_name = violation[:type].graphql_name
        location = path.empty? ? "the #{type_name} root object" : "#{type_name} at `#{path.join(".")}`"
        GraphQL::AnalysisError.new(
          "Query has response width of #{violation[:width]} on #{location}, which exceeds max width of #{configured_max_width}",
          extensions: {
            code: :max_width_exceeded,
            max_width: configured_max_width,
            response_width: violation[:width],
            type: type_name,
          }
        ).tap do |err|
          err.path = path
        end
      end
    end
  end
end
