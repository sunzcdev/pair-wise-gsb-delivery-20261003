# frozen_string_literal: true

module JMESPath
  module Errors
    class Error < StandardError; end

    class RuntimeError < Error; end

    class SyntaxError < Error; end

    class InvalidTypeError < Error; end

    class InvalidValueError < Error; end

    class InvalidArityError < Error; end

    class UnknownFunctionError < Error; end

    class StaticTypeError < Error
      attr_reader :result

      def initialize(result)
        @result = result
        lines = result.errors.map do |finding|
          location = finding.trail.empty? ? finding.node_class.name.split('::').last : finding.trail.join(' -> ')
          "  - #{finding.message} (at #{location})"
        end
        super("expression has #{result.errors.size} static type error(s):\n#{lines.join("\n")}")
      end
    end
  end
end
