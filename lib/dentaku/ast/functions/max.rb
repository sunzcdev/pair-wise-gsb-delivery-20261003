require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:max, :numeric, ->(*args) {
  args.flatten.map { |arg| Dentaku::Units.numeric_or_quantity(arg) }.max
})
