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
  require 'jmespath/shape'
  require 'jmespath/static_type_checker'
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

    # @api private
    def load_json(path)
      JSON.parse(File.open(path, 'r', encoding: 'UTF-8', &:read))
    end

    # Statically check an expression against a lightweight {Shape} description
    # of the input and infer the shape of the query result, without executing
    # the query against any data.
    #
    #   shape = JMESPath::Shape.object(age: JMESPath::Shape.number)
    #   result = JMESPath.check('abs(age)', shape)
    #   result.ok?            #=> true
    #   result.output_shape   #=> #<JMESPath::Shape number>
    #
    # Only *definite* failures are reported as errors. Anything that could
    # still succeed because the shape is open, unknown (+any+) or a wider
    # union is accepted and surfaced in `result.runtime_checks` instead.
    #
    # @param [String] expression
    # @param [Shape] input_shape
    # @param [Hash] options passed to {StaticTypeChecker#initialize}
    # @return [StaticTypeChecker::Result]
    def check(expression, input_shape, options = {})
      StaticTypeChecker.new(input_shape, options).check(expression)
    end

    # Same as {.check} but raises {Errors::StaticTypeError} when definite
    # compile-time type errors are found.
    #
    # @raise [Errors::StaticTypeError]
    # @return [StaticTypeChecker::Result]
    def check!(expression, input_shape, options = {})
      result = check(expression, input_shape, options)
      raise Errors::StaticTypeError, result unless result.ok?
      result
    end
  end
end
