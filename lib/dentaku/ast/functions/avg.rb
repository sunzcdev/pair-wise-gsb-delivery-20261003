require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:avg, :numeric, ->(*args) {
  flatten_args = args.flatten
  if flatten_args.empty?
    raise Dentaku::ArgumentError.for(
        :too_few_arguments,
        function_name: 'AVG', expected: 1.., actual: 0
    )
  end

  flatten_args.map { |arg| Dentaku::Units.numeric_or_quantity(arg) }.reduce(:+) / BigDecimal(flatten_args.length)
})
