# frozen_string_literal: true
# Demonstrates the difference between the default search semantics and
# the diagnostic mode. Run with:
#
#   ruby -Ilib examples/diagnose.rb
#
$LOAD_PATH.unshift(File.expand_path('../lib', __dir__))
require 'jmespath'
require 'json'

# `retries` is explicitly null, `timeout` is absent, and `service.tags`
# has an element without a `weight` key.
data = {
  'service' => {
    'endpoint' => 'https://example.com',
    'retries' => nil,
    'tags' => [{ 'name' => 'a' }, { 'name' => 'b', 'weight' => 2 }]
  }
}

expressions = [
  'service.retries',              # explicit null
  'service.timeout',              # missing key
  'service.timeout.seconds',      # missing in the middle of a deep path
  'service.timeout == `null`',    # missing compared to null
  'service.tags[*].weight',       # projection over an element missing a key
  'not_null(service.timeout, `3`)' # missing value as a function argument
]

expressions.each do |expression|
  puts expression
  puts "  search:   #{JMESPath.search(expression, data).inspect}"
  report = JMESPath.diagnose(expression, data)
  puts "  diagnose: #{JSON.generate(report)}"
  # The report must never leak the internal sentinel.
  raise 'sentinel leaked!' if JSON.generate(report).include?('MISSING')
end
