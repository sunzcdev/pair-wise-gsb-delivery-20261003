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
  require 'jmespath/runtime'
  require 'jmespath/sourced'
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

    # Searches the data, just like {search}, but also reports where every
    # value in the result came from in the original document.
    #
    #   result = JMESPath.search_with_sources('foo.bar', { 'foo' => { 'bar' => 1, 'baz' => 2 } })
    #   result.value                 #=> 1
    #   result.locations[''].pointer #=> "/foo/bar"
    #
    # Values copied verbatim carry a JSON Pointer. Computed values (function
    # results, literals, comparisons, multi-selects, projections, ...) are
    # marked as derived and list the JSON Pointers of their direct inputs.
    # Positions removed by projections (values resolved to `null`) simply do
    # not appear, so a result-array index is never mistaken for a source
    # array index.
    #
    # @param [String] expression A valid
    #   [JMESPath](https://github.com/boto/jmespath) expression.
    # @param [Hash] data
    # @return [SourcedResult]
    def search_with_sources(expression, data, runtime_options = {})
      data = case data
             when Hash, Struct then data
             when Pathname then load_json(data)
             when IO, StringIO then JSON.parse(data.read)
             else data
        end
      Runtime.new(runtime_options).search_with_sources(expression, data)
    end

    # @api private
    def load_json(path)
      JSON.parse(File.open(path, 'r', encoding: 'UTF-8', &:read))
    end
  end
end
