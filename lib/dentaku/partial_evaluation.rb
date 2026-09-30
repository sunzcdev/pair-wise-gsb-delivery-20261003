require 'dentaku/print_visitor'

module Dentaku
  # Outcome of a partial evaluation.
  #
  #   * +residual+ is an AST node: either a single folded constant or an
  #     expression tree that references the missing inputs
  #   * +missing_variables+ lists every input that was not supplied
  #   * +errors+ contains evaluation errors encountered while folding
  #
  # An explicitly supplied nil is an ordinary value, so it never appears
  # in +missing_variables+.
  class PartialEvaluation
    attr_reader :residual, :missing_variables, :errors

    def initialize(residual, missing_variables, errors)
      @residual = residual
      @missing_variables = missing_variables
      @errors = errors
    end

    # True when every input was present and every folded subtree
    # evaluated successfully: the residual is a single constant.
    def resolvable?
      missing_variables.empty? && errors.empty?
    end

    # The fully computed value, available when the expression could be
    # reduced to a single constant.
    def value
      raise Dentaku::UnboundVariableError.new(missing_variables),
            "no value provided for variables: #{missing_variables.uniq.join(', ')}" unless missing_variables.empty?

      raise errors.first unless errors.empty?

      residual.value
    end

    # Residual expression as a string that can be re-parsed later, once
    # the missing inputs have been provided.
    def to_s
      PrintVisitor.new(residual).to_s
    end
  end
end
