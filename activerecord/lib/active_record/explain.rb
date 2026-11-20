# frozen_string_literal: true

require "active_record/explain_registry"

module ActiveRecord
  module Explain
    # Runs the block with its own capture active and returns that capture.
    # Statements the block triggers are recorded there and attributed to their
    # owner (the main query, a preload, ...). When another capture is already
    # active the new one nests under it and its statements are not mixed into
    # the enclosing capture, so concurrent +explain+ calls stay separate.
    # The capture is released even if the block raises, leaving the registry
    # exactly as it was before the call.
    def collecting_queries_for_explain(owner = nil) # :nodoc:
      ExplainRegistry.capture(owner) { yield }
    end

    # Makes the adapter execute EXPLAIN for the tuples of queries and bindings.
    # Returns a formatted string ready to be logged.
    #
    # +queries+ may be a plain collection of +[sql, binds]+ pairs, or a
    # capture as returned by #collecting_queries_for_explain. A capture with
    # statements belonging to more than one owner is rendered in sections so
    # each query's plan is shown next to the phase that triggered it.
    def exec_explain(queries, options = []) # :nodoc:
      str = with_connection do |c|
        render_explain(c, queries, options)
      end
      # Overriding inspect to be more human readable, especially in the console.
      def str.inspect
        self
      end

      str
    end

    private
      def render_explain(connection, queries, options)
        if queries.is_a?(ExplainRegistry::Capture)
          render_capture(connection, queries, options)
        else
          render_query_plans(connection, queries, options).join("\n")
        end
      end

      # Renders a capture in the order the statements were recorded. When more
      # than one owner contributed (a main query plus its preloads, say), each
      # run of statements is preceded by the owner it belongs to, so the plan
      # for a preload is not mistaken for the plan of the main query. A single
      # owner keeps the historical unlabelled output.
      def render_capture(connection, capture, options)
        label = capture.owners.size > 1

        sections = []
        current_owner = nil
        capture.each do |query|
          if label && query.owner != current_owner
            sections << "-- #{query.owner || "query"}"
            current_owner = query.owner
          end
          sections << render_query_plan(connection, query.sql, query.binds, options)
        end
        sections.join("\n")
      end

      def render_query_plans(connection, queries, options)
        queries.map { |sql, binds| render_query_plan(connection, sql, binds, options) }
      end

      def render_query_plan(connection, sql, binds, options)
        msg = +"#{build_explain_clause(connection, options)} #{sql}"
        unless binds.empty?
          msg << " "
          msg << binds.map.with_index { |attr, i| render_bind(connection, attr, i) }.inspect
        end
        msg << "\n"
        msg << connection.explain(sql, binds, options)
      end

      def render_bind(connection, attr, i)
        if ActiveModel::Attribute === attr
          value = if attr.type.binary? && attr.value
            "<#{attr.value_for_database.to_s.bytesize} bytes of binary data>"
          else
            connection.type_cast(attr.value_for_database)
          end
          [attr.name, value]
        else
          ["$#{i + 1}", connection.type_cast(attr)]
        end
      end

      def build_explain_clause(connection, options = [])
        if connection.respond_to?(:build_explain_clause, true)
          connection.build_explain_clause(options)
        else
          "EXPLAIN for:"
        end
      end
  end
end
