# frozen_string_literal: true

module GraphQL
  module Language
    # Project-specific extension, **not** part of the GraphQL specification.
    #
    # Implements fragment arguments (sometimes called "fragment variables"):
    #
    # @example
    #   fragment ConnectionFields($first: Int = 10) on Friend {
    #     contacts(first: $first) { nodes { name } }
    #   }
    #
    #   query {
    #     me { ...ConnectionFields(first: 5) }
    #     bestFriend { ...ConnectionFields(first: 20) }
    #   end
    #
    # The expander rewrites such a document into an ordinary document that the
    # existing execution pipeline can handle: every parameterized fragment
    # spread is replaced with an inline fragment whose local `$variables` have
    # been substituted by the argument values (literals are inlined and outer
    # variables are passed through unchanged). Each spread is expanded from a
    # fresh copy, so two spreads of the same fragment with different arguments
    # never share an expansion.
    module FragmentArguments
      # Rewrite `document` so that fragment arguments are fully expanded into
      # ordinary GraphQL syntax.
      #
      # @param schema [GraphQL::Schema]
      # @param document [GraphQL::Language::Nodes::Document]
      # @param context [GraphQL::Query::Context, nil]
      # @return [Array(GraphQL::Language::Nodes::Document, Array<GraphQL::StaticValidation::Error>, Array<String>)]
      #   the (possibly rewritten) document, expansion errors, and the names of
      #   operation variables that were forwarded as fragment arguments
      def self.expand(schema, document, context: nil)
        expander = Expander.new(schema, context)
        new_document, errors = expander.expand(document)
        [new_document, errors, expander.forwarded_variable_names]
      end
    end
  end
end

require "graphql/language/fragment_arguments/expander"
