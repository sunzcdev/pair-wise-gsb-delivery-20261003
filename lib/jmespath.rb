# frozen_string_literal: true
require 'json'
require 'stringio'
require 'pathname'

module JMESPath
  require 'jmespath/caching_parser'
  require 'jmespath/errors'
  require 'jmespath/lexer'
  require 'jmespath/nodes'
  require 'jmespath/parser'
  require 'jmespath/query_set'
  require 'jmespath/runtime'
  require 'jmespath/token'
  require 'jmespath/token_stream'
  require 'jmespath/util'
  require 'jmespath/version'

  class << self
    # @param [String] expression A valid
    #   [JMESPath](https://github.com/boto/jmespath) expression.
    # @param [Hash] data
    # @return [Mixed,nil] Returns the matched values. Returns `nil` if the
    #   expression does not resolve inside `data`.
    def search(expression, data, runtime_options = {})
      data = case data
             when Hash, Struct then data # check for most common case first
             when Pathname then load_json(data)
             when IO, StringIO then JSON.parse(data.read)
             else data
        end
      Runtime.new(runtime_options).search(expression, data)
    end

    # Builds a reusable, compiled {QuerySet} of named expressions whose
    # structurally shared AST prefixes are traversed only once.
    #
    #   qs = JMESPath.query_set(adults: "people[?age > `18`].name")
    #   qs.search(data)
    #
    # @see QuerySet
    def query_set(queries = nil, **options, &block)
      QuerySet.new(queries, **options, &block)
    end

    # One-shot convenience: compile `queries`, search `data`, and return the
    # fused result. See {QuerySet#initialize} for the available options.
    def search_all(queries, data, options = {}, &block)
      QuerySet.new(queries, **options).search(data, &block)
    end

    # @api private
    def load_json(path)
      JSON.parse(File.open(path, 'r', encoding: 'UTF-8', &:read))
    end
  end
end
