require_relative '../function'

Dentaku::AST::Function.register(:round, :numeric, lambda { |numeric, places = 0|
  Dentaku::Units.unwrap_numeric(numeric).round(Dentaku::NumericParser.ensure_numeric!(places || 0).to_i)
})
