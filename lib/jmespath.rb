# frozen_string_literal: true
require 'json'
require 'stringio'
require 'pathname'

module JMESPath
  require 'jmespath/access_summary'
  require 'jmespath/caching_parser'
  require 'jmespath/errors'
  require 'jmespath/lexer'
  require 'jmespath/nodes'
  require 'jmespath/parser'
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

    # Statically analyzes a JMESPath expression and reports which data
    # fields it may read. The expression is compiled but never evaluated
    # against data. See {AccessSummary} for details.
    #
    #     summary = JMESPath.access_summary('users[?age > `20`].name')
    #     summary.required.map(&:to_s)
    #     #=> ["users (required, field, output)", ...]
    #
    # @param [String] expression A valid
    #   [JMESPath](https://github.com/boto/jmespath) expression.
    # @param [Hash] runtime_options Passed to {Runtime}.
    # @return [AccessSummary::Summary]
    def access_summary(expression, runtime_options = {})
      Runtime.new(runtime_options).access_summary(expression)
    end

    # @api private
    def load_json(path)
      JSON.parse(File.open(path, 'r', encoding: 'UTF-8', &:read))
    end
  end
end
