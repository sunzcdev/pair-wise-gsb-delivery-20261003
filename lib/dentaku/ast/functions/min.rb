require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:min, :numeric, ->(*args) {
  args.flatten.map { |arg| Dentaku::Units.numeric_or_quantity(arg) }.min
})
