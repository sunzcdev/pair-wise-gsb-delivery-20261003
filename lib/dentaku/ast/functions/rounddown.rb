require_relative '../function'
require 'dentaku/units'

Dentaku::AST::Function.register(:rounddown, :numeric, lambda { |numeric, precision = 0|
  precision = Dentaku::NumericParser.ensure_numeric!(precision || 0).to_i
  next numeric.floor(precision) if Dentaku::Units.quantity?(numeric)

  tens = 10.0**precision
  result = (Dentaku::NumericParser.ensure_numeric!(numeric) * tens).floor / tens
  precision <= 0 ? result.to_i : result
})
