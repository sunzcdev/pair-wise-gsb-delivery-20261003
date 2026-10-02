$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))

require "dry/types"

module Types
  include Dry.Types()
end

# --- 1. A self-referencing approval-condition tree -------------------------
#
# Leaves are comparisons, composite nodes have +children+ which are again
# condition nodes. The schema is named once and refers back to itself, so the
# infinite nesting never has to be written out.

condition = Dry::Types.recursive(:condition) do |r|
  comparison = Types::Hash.schema(
    op: Types::String.constrained(included_in: %w[eq ne gt lt gte lte]),
    field: Types::String,
    value: Types::Any
  )

  group = Types::Hash.schema(
    op: Types::String.constrained(included_in: %w[and or not]),
    children: Types::Array.of(r[:condition])
  )

  comparison | group
end

tree = {
  op: "and",
  children: [
    {op: "gte", field: "amount", value: 100},
    {
      op: "or",
      children: [
        {op: "eq", field: "region", value: "EU"},
        {op: "eq", field: "vip", value: true}
      ]
    }
  ]
}

puts "valid tree => #{condition.(tree) == tree}"

# A cyclic *input* value reports the exact position where it repeats.
cyclic = {op: "and", children: []}
cyclic[:children] << cyclic

begin
  condition.(cyclic)
rescue Dry::Types::Recursive::CyclicInputError => error
  puts "cyclic input at #{error.path.inspect}: #{Dry::Types::Recursive::Guard.format_path(error.path)}"
end

# A definition that never consumes input (r.x = r.x) is rejected deterministically.
broken = Dry::Types.recursive(:x) { |r| r[:x] }

begin
  broken.(1)
rescue Dry::Types::Recursive::NonProductiveRecursionError => error
  puts "non-productive definition rejected"
end

# --- 2. Mutually referencing types -----------------------------------------

group = Dry::Types::Recursive.define do |r|
  r[:pair] = Types::Hash.schema(left: r[:item], right: r[:item])
  r[:item] = Types::Integer | r[:pair]
end

puts "mutual recursion => #{group[:pair].(left: 1, right: {left: 2, right: 3}).inspect}"

# An unclosed reference is detected as soon as the block finishes.
begin
  Dry::Types::Recursive.define do |r|
    r[:a] = Types::Hash.schema(b: r[:never_defined])
  end
rescue Dry::Types::Recursive::UnresolvedReferenceError => error
  puts "unclosed reference: #{error.message}"
end

# --- 3. Stable AST export and rebuild --------------------------------------

ast = condition.to_ast
rebuilt = Dry::Types::Compiler.new(Dry::Types).(ast)

puts "AST root node      => #{ast.first}"
puts "AST rebuilds equal => #{rebuilt.to_ast == ast}"
puts "rebuilt coerces    => #{rebuilt.(tree) == tree}"
