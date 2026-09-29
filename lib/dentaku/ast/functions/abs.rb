require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:abs, :numeric, lambda { |numeric|
  Dentaku::Units.numeric_or_quantity(numeric).abs
})
