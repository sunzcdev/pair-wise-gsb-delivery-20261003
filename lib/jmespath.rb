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
  require 'jmespath/streaming_result'
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

    # Streams the results of a JMESPath expression over a read-only
    # array source, producing results as elements are read. See
    # {Runtime#stream} for details.
    #
    # @param [String] expression A valid
    #   [JMESPath](https://github.com/boto/jmespath) expression.
    # @param [#each] source A read-only, streaming array source.
    # @return [StreamingResult] A lazy, forward-only enumerable of
    #   results.
    def stream(expression, source, runtime_options = {})
      Runtime.new(runtime_options).stream(expression, source)
    end

    # @api private
    def load_json(path)
      JSON.parse(File.open(path, 'r', encoding: 'UTF-8', &:read))
    end
  end
end
