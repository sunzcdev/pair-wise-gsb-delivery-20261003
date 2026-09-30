require 'dentaku/ast'
require 'dentaku/print_visitor'

module Dentaku
  # The outcome of Calculator#evaluate_partial: a residual expression plus
  # everything needed to finish the calculation later.
  #
  # - +missing+ lists the identifiers that still need values (unknowns)
  # - +errors+ lists evaluation errors found in already-determined parts
  #   (an explicitly provided nil is a determined value, so it never shows
  #   up in +missing+; it folds to NULL and may surface here instead)
  # - +expression+ is the residual formula, printable with the original
  #   operator precedence and re-evaluable once the missing data is supplied
  class PartialEvaluation
    attr_reader :node, :missing, :errors

    def initialize(node, context, errors)
      @node = node
      @context = context
      @errors = errors
      @missing = node.dependencies(
        context.merge(AST::Node::STATIC_MODE_KEY => true)
      ).uniq
    end

    def complete?
      errors.empty? && missing.empty?
    end

    def value
      return nil unless complete?

      node.value(@context)
    end

    def expression
      PrintVisitor.new(node).to_s
    end

    def to_s
      expression
    end
  end
end
