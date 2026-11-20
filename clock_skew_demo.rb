# frozen_string_literal: true

# Demo: how the OpenTelemetry Ruby SDK mixes explicit (caller-supplied)
# timestamps with automatic (SDK-measured) timestamps, and what happens
# when the system realtime clock is stepped backwards mid-span.
#
# Two controllable clocks are used:
#   * REALTIME   - simulates the wall clock (can be stepped backwards, e.g. NTP)
#   * MONOTONIC  - simulates the monotonic clock (only moves forward)

class FakeClock
  attr_accessor :now_ns

  def initialize(now_ns) = @now_ns = now_ns
  def advance_ns(ns) = @now_ns += ns
  def advance_s(s) = @now_ns += (s * 1_000_000_000).to_i
end

REALTIME  = FakeClock.new(1_763_700_000_000_000_000) # fixed epoch, ns
MONOTONIC = FakeClock.new(5_000_000_000_000)         # arbitrary monotonic base

module ClockStub
  def clock_gettime(clock_id, unit = :float_second)
    if unit == :nanosecond
      return REALTIME.now_ns  if clock_id == Process::CLOCK_REALTIME
      return MONOTONIC.now_ns if clock_id == Process::CLOCK_MONOTONIC
    end

    super
  end
end
Process.singleton_class.prepend(ClockStub)

require 'opentelemetry/sdk'

NS = 1_000_000_000

def t(ns) = (ns / NS.to_f).round(6) # ns -> seconds, readable output

exporter = OpenTelemetry::SDK::Trace::Export::InMemorySpanExporter.new
provider = OpenTelemetry::SDK::Trace::TracerProvider.new
provider.add_span_processor(OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor.new(exporter))
tracer = provider.tracer('clock-demo')

$failures = []
def check(desc, actual, expected)
  ok = actual == expected
  $failures << desc unless ok
  puts format('  %-72s %s', "#{desc}: #{actual.inspect} (expected #{expected.inspect})", ok ? 'OK' : 'FAIL')
end

puts '=== Scenario A: explicit start + auto event/finish, realtime clock rolls back ==='

# Caller recorded "request received" 2s before the span is created.
caller_start_ns = REALTIME.now_ns - 2 * NS
caller_start    = Time.at(Rational(caller_start_ns, NS))

anchor_realtime_ns = REALTIME.now_ns # what the SDK will capture as realtime anchor

span = tracer.start_span('explicit-start', start_timestamp: caller_start)

MONOTONIC.advance_s(3)
REALTIME.advance_s(-3600) # NTP steps the wall clock back 1 hour!
span.add_event('mid')     # automatic timestamp

MONOTONIC.advance_s(2)
REALTIME.advance_s(-1800) # and back another 30 minutes
span.finish               # automatic end_timestamp

sd = exporter.finished_spans.last
puts "  SpanData start: #{t(sd.start_timestamp)}  end: #{t(sd.end_timestamp)}  event: #{t(sd.events[0].timestamp)}"
puts "  wall clock now reads: #{t(REALTIME.now_ns)} (rolled back 1.5h during the span)"

check('start == explicit caller timestamp', sd.start_timestamp, caller_start_ns)
check('event == realtime_anchor + 3s monotonic', sd.events[0].timestamp, anchor_realtime_ns + 3 * NS)
check('end   == realtime_anchor + 5s monotonic', sd.end_timestamp, anchor_realtime_ns + 5 * NS)
check('duration == 7s (2s pre-span gap + 5s monotonic)', sd.end_timestamp - sd.start_timestamp, 7 * NS)
check('start < event < end ordering holds', sd.start_timestamp < sd.events[0].timestamp && sd.events[0].timestamp < sd.end_timestamp, true)
puts

puts '=== Scenario B: explicit event/finish earlier than start (no clamping) ==='
exporter.reset

t0 = Time.at(Rational(REALTIME.now_ns, NS))
span = tracer.start_span('weird-times', start_timestamp: t0)
span.add_event('too-early', timestamp: t0 - 10)   # 10s BEFORE the span start
span.add_event('auto-after')                      # automatic timestamp
span.finish(end_timestamp: t0 - 1)                # ends 1s BEFORE it started

sd = exporter.finished_spans.last
start_ns = (t0.to_r * NS).to_i
puts "  SpanData start: #{t(sd.start_timestamp)}  end: #{t(sd.end_timestamp)}"
puts "  events: #{sd.events.map { |e| "#{e.name}@#{t(e.timestamp)}" }.join(', ')}"

check('early event recorded verbatim (10s before start)', sd.events[0].timestamp, start_ns - 10 * NS)
check('end recorded verbatim (1s before start)', sd.end_timestamp, start_ns - 1 * NS)
check('negative duration surfaces as-is', sd.end_timestamp - sd.start_timestamp, -1 * NS)
check('events keep insertion order, not time order', sd.events.map(&:name), %w[too-early auto-after])
puts

puts '=== Scenario C: child span inherits parent anchor under rollback ==='
exporter.reset

parent_anchor = REALTIME.now_ns
parent = tracer.start_span('parent')
MONOTONIC.advance_s(1)
REALTIME.advance_s(-600) # clock steps back 10 minutes before the child starts
child = tracer.start_span('child', with_parent: OpenTelemetry::Trace.context_with_span(parent))
MONOTONIC.advance_s(1)
child.finish
parent.finish

spans = exporter.finished_spans
child_sd = spans.find { |s| s.name == 'child' }
parent_sd = spans.find { |s| s.name == 'parent' }
check('parent start == anchor', parent_sd.start_timestamp, parent_anchor)
check('child start == parent anchor + 1s monotonic', child_sd.start_timestamp, parent_anchor + 1 * NS)
check('child end   == parent anchor + 2s monotonic', child_sd.end_timestamp, parent_anchor + 2 * NS)
check('parent end  == parent anchor + 2s monotonic', parent_sd.end_timestamp, parent_anchor + 2 * NS)
puts

provider.shutdown

if $failures.empty?
  puts 'ALL CHECKS PASSED'
else
  puts "#{$failures.size} FAILURES:"
  $failures.each { |f| puts "  - #{f}" }
  exit 1
end
