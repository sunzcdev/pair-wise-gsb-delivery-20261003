require_relative '../function'

Dentaku::AST::Function.register(:max, :numeric, ->(*args) {
  args.flatten.map { |arg| Dentaku::Units.unwrap_numeric(arg) }.max
})
