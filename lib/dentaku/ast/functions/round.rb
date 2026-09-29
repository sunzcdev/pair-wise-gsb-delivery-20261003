require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:round, :numeric, lambda { |numeric, places = 0|
  Dentaku::Units.numeric_or_quantity(numeric).round(Dentaku::NumericParser.ensure_numeric!(places || 0).to_i)
})
