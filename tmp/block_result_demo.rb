# frozen_string_literal: true

# Demonstration of custom_block_results: matching priority, already-written
# response timing, frozen parent / independent subclass hashes, explicit
# hand-back to ordinary handling, and String vs full Rack response results.

$LOAD_PATH.unshift File.expand_path("../lib", __dir__)
require "stringio"
require "roda"

$log = []

# A gateway payload that is itself a Hash, so two kinds of plugin register
# handlers whose === predicates both match it:
#   * :json registers Hash  -> :handle_json_block_result   (generic handler)
#   * our gateway code registers Report -> a specific handler
class Report < Hash
  def initialize(payload)
    merge!(payload)
  end
end

# 1) Frozen parent app. Plugins are loaded BEFORE subclassing and BEFORE
#    freeze, because Roda forbids subclassing a frozen app and forbids
#    loading plugins into a frozen app.
class Base < Roda
  plugin :json                 # pulls in custom_block_results, registers Array/Hash
  plugin :symbol_views         # also uses custom_block_results, registers Symbol

  # Generic gateway handler installed on the parent for Report.
  handle_block_result(Report) do |report|
    $log << "Base#handle_report"
    response["X-Handler"] = "base-report"
    "base-report:#{report[:id]}"
  end

  route { |_| }
end

# 2) Two independent subclasses, created while the parent is still mutable.
class GatewayA < Base; end
class GatewayB < Base; end

# GatewayA wants the *specific* Report handler to win over the generic Hash
# (JSON) handler. Iteration order of the custom_block_results hash IS the
# priority: unsupported_block_result picks the FIRST key whose === matches.
# Inherited order is: Array, Hash, Symbol, Report. Reorder in A's OWN hash
# copy (inherited dup, because the parent hash was not frozen yet).
a_hash = GatewayA.custom_block_results
report_method = a_hash.delete(Report)
rebuilt = { Report => report_method }
a_hash.each { |k, v| rebuilt[k] = v }
GatewayA.opts[:custom_block_results] = rebuilt
GatewayA.instance_variable_set(:@opt_custom_block_results, rebuilt)

# Remember the ordinary parent handler before overriding it. Generated roda
# method names are per-class counters, so Base and GatewayA happen to use the
# same method name; a plain send(name) would therefore self-recurse. Capture
# the actual parent method object and bind it to this instance instead.
parent_report_method = Base.custom_block_results[Report]
parent_report_handler = Base.instance_method(parent_report_method)

# GatewayA overrides the inherited Report method with a specific handler,
# and can explicitly hand a request back to the ordinary (parent) handler.
a_method = GatewayA.handle_block_result(Report) do |report|
  $log << "GatewayA#handle_report"
  if report[:delegate]
    # Explicit hand-back: run the ordinary handler inherited from Base.
    next parent_report_handler.bind_call(self, report)
  end
  response["X-Handler"] = "gateway-a"
  "a-specific:#{report[:id]}"
end
puts "GatewayA Report handler method: #{a_method.inspect}"
puts "Base     Report handler method: #{parent_report_method.inspect}"

# GatewayB keeps the inherited order unchanged: Hash (JSON) sits before
# Report, so a Report matches the generic JSON handler first. It also adds
# its own Report handler, but that handler is unreachable while Hash stays
# ahead of Report in iteration order.
GatewayB.handle_block_result(Report) do |report|
  $log << "GatewayB#handle_report (unreachable in inherited order)"
  "b-specific:#{report[:id]}"
end

# Now freeze the parent. Its custom_block_results hash is frozen too and its
# order can never again be rearranged.
Base.freeze

puts "=" * 72
puts "SECTION 1: registered handlers and frozen state"
puts "=" * 72
puts "Base frozen?=#{Base.frozen?}  hash frozen?=#{Base.custom_block_results.frozen?}"
puts "  Base order: #{Base.custom_block_results.keys.inspect}"
puts "  A    order: #{GatewayA.custom_block_results.keys.inspect}"
puts "  B    order: #{GatewayB.custom_block_results.keys.inspect}"
puts "  identical hash objects? Base==A: #{Base.custom_block_results.equal?(GatewayA.custom_block_results)}"
puts "                           Base==B: #{Base.custom_block_results.equal?(GatewayB.custom_block_results)}"
puts "                           A==B   : #{GatewayA.custom_block_results.equal?(GatewayB.custom_block_results)}"

begin
  Base.handle_block_result(Integer) { |i| i }
rescue => e
  puts "mutating frozen parent raises: #{e.class}: #{e.message}"
end
begin
  Base.custom_block_results[Integer] = :x
rescue => e
  puts "inserting into frozen hash raises: #{e.class}: #{e.message}"
end
begin
  Class.new(Base)
rescue => e
  puts "subclassing frozen parent raises: #{e.class}: #{e.message}"
end
puts "B still has generic Hash ahead of Report: " \
     "#{GatewayB.custom_block_results.keys.index(Hash) < GatewayB.custom_block_results.keys.index(Report)}"

# Run an app with a given route block result; collect the Rack triplet.
def probe(app, result)
  $log = []
  app.route { |_| result }
  status, headers, body = app.call(
    "REQUEST_METHOD" => "GET",
    "PATH_INFO" => "/",
    "SCRIPT_NAME" => "",
    "rack.input" => String.new,
    "rack.errors" => StringIO.new
  )
  text = +""
  body.each { |chunk| text << chunk }
  body.close if body.respond_to?(:close)
  [status, headers, text, $log.dup]
end

def show(title, status, headers, body, log)
  puts "-" * 72
  puts title
  puts "  status   : #{status}"
  puts "  c-type   : #{headers['content-type'].inspect}"
  puts "  x-handler: #{headers['X-Handler'].inspect}"
  puts "  body     : #{body.inspect}"
  puts "  handlers : #{log.inspect}"
end

report = Report.new(id: 7, kind: "gateway")

puts
puts "=" * 72
puts "SECTION 2: both subclass apps process the SAME object once each"
puts "=" * 72
show("GatewayA.call(same_report)", *probe(GatewayA, report.dup))
show("GatewayB.call(same_report)", *probe(GatewayB, report.dup))

puts
puts "=" * 72
puts "SECTION 3: explicit hand-back to the ordinary (parent) handler"
puts "=" * 72
show("GatewayA.call(report with delegate:true)",
     *probe(GatewayA, Report.new(id: 9, delegate: true)))

puts
puts "=" * 72
puts "SECTION 4: String results use Roda's built-in path, never custom ones"
puts "=" * 72
show("GatewayA.call(String)", *probe(GatewayA, "plain-string-body"))
show("GatewayB.call(String)", *probe(GatewayB, "plain-string-body"))

puts
puts "=" * 72
puts "SECTION 5: full Rack response array result"
puts "=" * 72
rack_response = [202, {"content-type" => "application/x-custom", "X-Handler" => "raw-rack"}, ["rack-array-body"]]
show("GatewayA.call(full Rack response RETURNED)", *probe(GatewayA, rack_response))
show("GatewayB.call(full Rack response RETURNED)", *probe(GatewayB, rack_response))

# Contrast: r.halt with a Rack triplet short-circuits via throw(:halt) and
# never reaches block-result classification at all.
def probe_halt(app, rack_response)
  $log = []
  app.route { |r| r.halt(rack_response) }
  status, headers, body = app.call(
    "REQUEST_METHOD" => "GET", "PATH_INFO" => "/", "SCRIPT_NAME" => "",
    "rack.input" => String.new, "rack.errors" => StringIO.new
  )
  text = +""; body.each { |c| text << c }
  body.close if body.respond_to?(:close)
  [status, headers, text, $log.dup]
end
show("GatewayA r.halt(full Rack response)", *probe_halt(GatewayA, rack_response))
show("GatewayB r.halt(full Rack response)", *probe_halt(GatewayB, rack_response))

puts
puts "=" * 72
puts "SECTION 6: already-written response: block result is ignored"
puts "=" * 72
[GatewayA, GatewayB].each do |app|
  $log = []
  app.route do |r|
    response.write "written-first"   # body no longer empty?
    report.dup                       # return value now ignored
  end
  status, headers, body = app.call(
    "REQUEST_METHOD" => "GET", "PATH_INFO" => "/", "SCRIPT_NAME" => "",
    "rack.input" => String.new, "rack.errors" => StringIO.new
  )
  text = +""; body.each { |c| text << c }
  body.close if body.respond_to?(:close)
  puts "#{app.name}: status=#{status} body=#{text.inspect} handlers_called=#{$log.inspect}"
end

puts
puts "=" * 72
puts "SECTION 7: timing detail - even an empty-string write flips empty?"
puts "=" * 72
$log = []
GatewayA.route do |r|
  response.write ""                  # empty? becomes false even for ""
  report.dup
end
status, headers, body = GatewayA.call(
  "REQUEST_METHOD" => "GET", "PATH_INFO" => "/", "SCRIPT_NAME" => "",
  "rack.input" => String.new, "rack.errors" => StringIO.new
)
text = +""; body.each { |c| text << c }
body.close if body.respond_to?(:close)
puts "body=#{text.inspect} status=#{status} handlers_called=#{$log.inspect}"

puts
puts "=" * 72
puts "SECTION 8: unhandled type still raises RodaError (ordinary fallback)"
puts "=" * 72
begin
  probe(GatewayA, 12345)
rescue => e
  puts "Integer result with no handler: #{e.class}: #{e.message}"
end
