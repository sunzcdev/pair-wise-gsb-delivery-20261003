# frozen_string_literal: true

module Grape
  class Entity
    class PartialContext
      attr_reader :errors

      def initialize
        @errors = []
      end

      def add(path, exception)
        errors << { path: path.dup, cause: exception }
      end
    end

    PartialRepresentation = Struct.new(:representation, :errors, keyword_init: true) do
      def to_ary
        [representation, errors]
      end
    end

    PARTIAL_OMIT = Object.new.freeze
  end
end
