# frozen_string_literal: true

# Demonstration of the lightweight compile-time shape checker.
#
# Run with:
#
#   ruby -Ilib examples/shape_check_demo.rb
#
# The document deliberately contains both *optional* object fields and
# *mixed* (heterogeneous) arrays, so we can contrast:
#
#   * errors the checker finds before the query ever runs, and
#   * things that genuinely depend on the concrete data and stay runtime.

$LOAD_PATH.unshift(File.join(__dir__, '..', 'lib'))
require 'jmespath'

# ----------------------------------------------------------------------
# 1. Describe the input shape (a lightweight alternative to JSON Schema).
# ----------------------------------------------------------------------

document_shape = JMESPath::Shape.object(
  team: JMESPath::Shape.array(
    JMESPath::Shape.object(
      id: JMESPath::Shape.number,                 # required, number
      name: JMESPath::Shape.string,               # required, string
      nickname: JMESPath::Shape.optional(         # may be missing
        JMESPath::Shape.string
      ),
      age: JMESPath::Shape.optional(              # may be missing
        JMESPath::Shape.number
      ),
      manager: JMESPath::Shape.optional(          # optional and nullable
        JMESPath::Shape.union(JMESPath::Shape.string, JMESPath::Shape.null)
      ),
      pets: JMESPath::Shape.optional(             # mixed array:
        JMESPath::Shape.array(                    # object variants ...
          JMESPath::Shape.union(
            JMESPath::Shape.object(
              kind: JMESPath::Shape.string,
              name: JMESPath::Shape.string,
              meows: JMESPath::Shape.boolean
            ),
            JMESPath::Shape.object(
              kind: JMESPath::Shape.string,
              name: JMESPath::Shape.string,
              barks: JMESPath::Shape.boolean
            )
          )
        )
      ),
      scores: JMESPath::Shape.optional(           # mixed scalar array
        JMESPath::Shape.array(
          JMESPath::Shape.union(JMESPath::Shape.number, JMESPath::Shape.string)
        )
      )
    )
  )
)

# One concrete document that satisfies the shape.
document = {
  'team' => [
    { 'id' => 1, 'name' => 'Ada', 'age' => 36,
      'pets' => [{ 'kind' => 'cat', 'name' => 'Pixel', 'meows' => true }],
      'scores' => [10, 7, 9] },
    { 'id' => 2, 'name' => 'Lin', 'nickname' => 'link',
      'manager' => nil,
      'pets' => [{ 'kind' => 'dog', 'name' => 'Rex', 'barks' => true }, 'a plain string'],
      'scores' => ['B'] },
    { 'id' => 3, 'name' => 'Moe' } # no age, nickname, pets, scores
  ]
}

puts "Input shape:"
puts "  #{document_shape}"
puts

def report(query, shape, document)
  result = JMESPath.check(query, shape)
  puts "Query: #{query}"
  puts "  output shape : #{result.output_shape}"
  if result.ok?
    puts "  compile check: ok"
  else
    result.errors.each do |finding|
      puts "  COMPILE ERROR: #{finding.message}"
      finding.reasons.each { |reason| puts "      - #{reason}" }
    end
  end
  result.runtime_checks.each do |finding|
    puts "  runtime kept : #{finding.message}"
  end

  executed =
    begin
      JMESPath.search(query, document).inspect
    rescue StandardError => error
      "raises #{error.class.name.split('::').last}: #{error.message}"
    end
  puts "  actual result: #{executed}"
  puts
end

puts "===== Optional fields ====="
puts

# Optional field reads as (type | null); output shape carries the null even
# though the query itself is fine.
report('team[*].age', document_shape, document)

# A filter removes the null branch; the projected `age` values are then all
# numbers. map() applies an expression element-wise:
report('team[?age != `null`] | [0].age', document_shape, document)
report('map(&age, team[?age != `null`])', document_shape, document)

# Chained on a narrowed element directly: after the filter, indexing the
# result array and applying abs() is safe (runtime bounds note only).
report('abs(team[?age != `null`] | [0].age)', document_shape, document)

# Same abs() WITHOUT the narrowing filter: the argument is number | null,
# which could fail, so it stays a runtime check rather than a compile error.
report('abs(team[0].age)', document_shape, document)

# Definitely wrong: nickname is a string, abs() needs a number. Every concrete
# possibility fails -> found before execution.
report('abs(team[0].nickname)', document_shape, document)

# Open object: an undeclared field is not an error; the lookup is kept.
report('team[*].shoe_size', document_shape, document)

puts "===== Mixed arrays ====="
puts

# Mixed scalar array: max() can succeed or fail depending on the actual
# elements. Element 1 here really is mixed (['B'] is homogeneous; use a member
# whose data contains both kinds), so no compile error and the mismatch is
# deferred to runtime.
mixed_document = {
  'team' => [
    { 'id' => 1, 'name' => 'Ada', 'scores' => [10, 'A', 9] },
    { 'id' => 2, 'name' => 'Lin' }
  ]
}
report('max(team[0].scores)', document_shape, mixed_document)

# A homogeneous string array would be provably wrong for sum(); build a shape
# where the elements are definitely strings to show a compile-time catch.
string_array = JMESPath::Shape.object(
  tags: JMESPath::Shape.array(JMESPath::Shape.string)
)
report('sum(tags)', string_array, { 'tags' => %w[a b] })

# Mixed object array (cat | dog): reading a field only one variant has yields
# a union; a function applied to it cannot be proved wrong.
report('team[*].pets[*].meows', document_shape, document)

# Sorting objects (not numbers/strings) inside a homogeneous array IS provably
# wrong -> compile error.
people_array = JMESPath::Shape.object(
  people: JMESPath::Shape.array(JMESPath::Shape.object(age: JMESPath::Shape.number))
)
report('sort(people)', people_array, { 'people' => [{ 'age' => 1 }] })

# Unknown data never triggers a false positive: with a plain field chain over
# `any`, nothing is proven and the check is deferred. Here the concrete data
# happens to contain a number, so the kept runtime check succeeds.
report('abs(anything.deep.weird)', JMESPath::Shape.any,
       { 'anything' => { 'deep' => { 'weird' => 42 } } })
# In contrast, feeding a *definite array* to abs() IS provably wrong, even
# when the array's own contents are unknown.
report('abs(things[*])', JMESPath::Shape.object(things: JMESPath::Shape.array(JMESPath::Shape.any)),
       { 'things' => [] })

puts "===== Arity and argument kinds ====="
puts
report('abs(`1`, `2`)', document_shape, document)   # wrong arity
report('starts_with(team[0].id, `x`)', document_shape, document) # number vs string
report('join(`,`, team[*].id)', document_shape, document)        # array of numbers
report('length(team)', document_shape, document)                 # array -> ok
