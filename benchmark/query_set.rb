# frozen_string_literal: true
#
# Demonstrates the named query-set API: extracting many fields from a
# document that contains one large array, comparing the fused plan against
# running the searches one by one. Run with:
#
#   ruby -Ilib benchmark/query_set.rb
#
require 'jmespath'

N = (ENV['SIZE'] || 5_000).to_i

data = {
  'company' => { 'name' => 'ACME', 'founded' => 1952 },
  'people' => Array.new(N) do |i|
    {
      'id'     => i,
      'name'   => "user#{i}",
      'email'  => "user#{i}@acme.example",
      'age'    => 18 + (i % 55),
      'city'   => i.even? ? 'NY' : 'LA',
      'active' => (i % 7) != 0,
      'score'  => (i * 13) % 100,
      'tags'   => i % 3 == 0 ? %w[vip] : [],
      'dept'   => { 'code' => "D#{i % 9}" }
    }
  end
}

# A realistic "one response, a dozen extracted fields" workload. Most of
# these re-filter / re-project the same big `people` array, each with its
# own continuation or predicate. Individually every one walks the array.
QUERIES = {
  company:       'company.name',
  founded:       'company.founded',
  headcount:     'length(people)',
  names:         'people[*].name',
  emails:        'people[*].email',
  dept_codes:    'people[*].dept.code',
  adult_names:   'people[?age >= `18`].name',
  adult_emails:  'people[?age >= `18`].email',
  adult_ids:     'people[?age >= `18`].id',
  seniors:       'people[?age >= `60`].[name, age]',
  ny_people:     'people[?city == `NY`].name',
  active_emails: 'people[?active == `true`].email',
  vip_names:     'people[?tags].name'
}.freeze

# ---- correctness: fused vs one-by-one -------------------------------------
qs = JMESPath.query_set(QUERIES)
fused = qs.search(data)

mismatches = QUERIES.count { |name, expression| fused[name] != JMESPath.search(expression, data) }
abort "correctness check failed (#{mismatches} mismatches)" unless mismatches.zero?

# ---- access counts ---------------------------------------------------------
stats = fused.stats
outer_traversals = QUERIES.count { |_, e| e.include?('people[') }

puts "array size: #{N}"
puts
puts "Access counts: fused query set vs independent JMESPath.search x#{QUERIES.size}"
puts '-' * 70
puts '%-28s %12s %12s %12s' % ['', 'separate', 'fused', 'saved']
puts '%-28s %12d %12d %12d' % ['AST node visits',
                               stats.separate.visits, stats.fused.visits, stats.visits_saved]
puts '%-28s %12d %12d %12d' % ['array element iterations',
                               stats.separate.array_iterations,
                               stats.fused.array_iterations,
                               stats.iterations_saved]
puts
puts "#{outer_traversals} expressions each traverse the #{N}-element `people` array on their"
puts "own (#{outer_traversals * N} element iterations). The fused query set identifies the shared"
puts "AST prefix and walks the whole array exactly once (#{stats.fused.array_iterations} iterations),"
puts "branching out to the different predicates/fields while it does so."
puts
puts "correctness: all #{QUERIES.size} fields match independent JMESPath.search"