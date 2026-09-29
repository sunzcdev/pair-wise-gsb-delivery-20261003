require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:sum, :numeric, ->(*args) {
  if args.empty?
    raise Dentaku::ArgumentError.for(
        :too_few_arguments,
        function_name: 'SUM', expected: 1.., actual: 0
    )
  end

  values = args.flatten.map { |arg| Dentaku::Units.numeric_or_quantity(arg) }
  values.empty? ? 0 : values.reduce(:+)
})
