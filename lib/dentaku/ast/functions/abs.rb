require_relative '../function'

Dentaku::AST::Function.register(:abs, :numeric, lambda { |numeric|
  Dentaku::Units.unwrap_numeric(numeric).abs
})
