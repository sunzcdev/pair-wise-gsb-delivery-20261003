require_relative '../function'

Dentaku::AST::Function.register(:rounddown, :numeric, lambda { |numeric, precision = 0|
  precision = Dentaku::NumericParser.ensure_numeric!(precision || 0).to_i
  if numeric.is_a?(Dentaku::Units::Quantity)
    next numeric.rounddown(precision)
  end

  tens = 10.0**precision
  result = (Dentaku::NumericParser.ensure_numeric!(numeric) * tens).floor / tens
  precision <= 0 ? result.to_i : result
})
