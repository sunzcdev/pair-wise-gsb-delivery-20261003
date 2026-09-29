# frozen_string_literal: true
require 'json'
require 'stringio'
require 'pathname'

module JMESPath
  require 'jmespath/caching_parser'
  require 'jmespath/missing'
  require 'jmespath/diagnostic_context'
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
      Runtime.new(runtime_options).search(expression, normalize_data(data))
    end

    # Searches data in diagnostic mode, distinguishing keys that are
    # absent from keys explicitly set to `null`.
    #
    # @param [String] expression A valid
    #   [JMESPath](https://github.com/boto/jmespath) expression.
    # @param [Hash] data
    # @return [Hash] A JSON serializable report with a `"value"` key and
    #   a `"diagnostics"` key. See {Runtime#diagnose}.
    def diagnose(expression, data, runtime_options = {})
      Runtime.new(runtime_options).diagnose(expression, normalize_data(data))
    end

    # @api private
    def load_json(path)
      JSON.parse(File.open(path, 'r', encoding: 'UTF-8', &:read))
    end

    # @api private
    def normalize_data(data)
      case data
      when Hash, Struct then data # check for most common case first
      when Pathname then load_json(data)
      when IO, StringIO then JSON.parse(data.read)
      else data
      end
    end
  end
end
