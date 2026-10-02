# frozen_string_literal: true
module JMESPath
  # @api private
  module Nodes
    # A pipe expression is semantically a subexpression at evaluation time,
    # but it is compiled as a distinct subclass so that downstream tooling
    # (e.g. the query-set fuser) can tell a pipe boundary apart from a dot
    # boundary. The two must never be conflated by textual prefix matching.
    class Pipe < Subexpression
      def source_marker
        :pipe
      end
    end
  end
end
