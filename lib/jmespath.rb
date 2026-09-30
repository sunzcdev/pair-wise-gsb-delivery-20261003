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
  require 'jmespath/token'
  require 'jmespath/token_stream'
  require 'jmespath/types'
  require 'jmespath/type_checker'
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

    # Compiles `expression` against a lightweight input `shape` and
    # reports function type errors that can be proven before runtime.
    # Anything that cannot be proven is left to the runtime checks.
    #
    #     result = JMESPath.check_types('abs(email)', 'email?' => :string)
    #     result.ok?         #=> false
    #     result.errors      #=> [#<JMESPath::TypeChecker::Error ...>]
    #     result.output_type #=> number
    #
    # @param [String] expression A valid JMESPath expression.
    # @param [Hash, Types::Type] shape The expected input shape. See
    #   {Types} for the shape description format.
    # @return [TypeChecker::Result]
    def check_types(expression, shape, runtime_options = {})
      TypeChecker.check(expression, shape, runtime_options)
    end

    # Like {.check_types}, but raises {Errors::TypeCheckError} when the
    # expression provably violates the given input `shape`.
    #
    # @param [String] expression A valid JMESPath expression.
    # @param [Hash, Types::Type] shape The expected input shape.
    # @return [TypeChecker::Result]
    # @raise [Errors::TypeCheckError]
    def assert_types!(expression, shape, runtime_options = {})
      check_types(expression, shape, runtime_options).raise_on_error!
    end
  end
end
