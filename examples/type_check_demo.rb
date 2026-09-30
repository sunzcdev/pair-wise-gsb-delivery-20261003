# frozen_string_literal: true
# Demo: compile-time shape checking vs. actual runtime behavior.
#
#   ruby -Ilib examples/type_check_demo.rb
#
# The input shape below uses optional fields (`email`, `address`,
# `users[].score`) and a mixed array (`scores` may contain numbers and
# strings). Each query is first checked at "compile time" against the
# shape, then executed against two real documents.

$LOAD_PATH.unshift File.expand_path('../lib', __dir__)
require 'jmespath'

SHAPE = JMESPath::Types.object(
  'name' => :string,
  'email' => [:maybe, :string],              # optional field
  'age' => :number,
  'tags' => [:string],                       # array<string>
  'scores' => [[:or, :number, :string]],     # mixed array<number|string>
  'address' => [:maybe, {                    # optional nested object
    'city' => :string,
    'zip' => [:maybe, :string]
  }],
  'users' => [{                              # array of objects
    'kind' => :string,
    'score' => [:maybe, :number]             # optional field on elements
  }]
)

# All optional fields present, scores all numeric.
DATA_FULL = {
  'name' => 'alice',
  'email' => 'alice@example.com',
  'age' => 30,
  'tags' => %w[admin ops],
  'scores' => [91, 82, 77],
  'address' => { 'city' => 'Shanghai', 'zip' => '200000' },
  'users' => [
    { 'kind' => 'admin', 'score' => 10 },
    { 'kind' => 'ops', 'score' => 20 }
  ]
}.freeze

# Optional fields missing, scores mixed with strings.
DATA_SPARSE = {
  'name' => 'bob',
  'age' => 41,
  'tags' => %w[ops],
  'scores' => [91, 'N/A', 88],
  'users' => [
    { 'kind' => 'admin' }, # score missing
    { 'kind' => 'ops', 'score' => 5 }
  ]
}.freeze

QUERIES = [
  'length(name)',
  'abs(age)',
  'abs(email)',
  'length(email)',
  'abs(address.zip)',
  'length(address.city)',
  'sum(tags)',
  'sum(scores)',
  "join(',', scores)",
  'users[].score',
  'users[?score != `null`].[score]',
  'users[?score == `null`].[score] | abs([0])'
].freeze

def run(query, data)
  JMESPath.search(query, data).inspect
rescue JMESPath::Errors::Error => e
  "#{e.class.name.split('::').last}: #{e.message}"
end

puts "input shape: #{JMESPath::Types.build(SHAPE)}"
puts
QUERIES.each do |query|
  result = JMESPath.check_types(query, SHAPE)
  puts "query: #{query}"
  puts "  inferred output type : #{result.output_type}"
  if result.ok?
    puts '  compile check        : ok (no provable error)'
  else
    puts '  compile check        : FAILED'
    result.errors.each { |e| puts e.to_s.gsub(/^/, '    ') }
  end
  puts "  runtime (full data)  : #{run(query, DATA_FULL)}"
  puts "  runtime (sparse data): #{run(query, DATA_SPARSE)}"
  puts
end
