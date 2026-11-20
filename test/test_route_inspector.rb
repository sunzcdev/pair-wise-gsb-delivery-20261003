require_relative 'helper'
require 'fluent/event_router'
require 'fluent/route_inspector'
require_relative 'test_plugin_classes'

class RouteInspectorTest < ::Test::Unit::TestCase
  include Fluent
  include FluentTest

  setup do
    @output = FluentTestOutput.new
    @other_output = FluentTestOutput.new
    @wide_output = FluentTestOutput.new
    @catch_all_output = FluentTestOutput.new
    @filter_a = FluentTestFilter.new('__a__')
    @filter_b = FluentCompatTestFilter.new('__b__')
    @emit_handler = TestEmitErrorHandler.new
    @default_collector = FluentTestOutput.new
    @event_router = EventRouter.new(@default_collector, @emit_handler)
    @inspector = RouteInspector.new(@event_router)
  end

  sub_test_case 'trace_route with overlapping patterns' do
    setup do
      # declaration order = EventRouter lookup order
      @event_router.add_rule('app.**', @filter_a)               # 0: filter, wide
      @event_router.add_rule('app.web.**', @filter_b)           # 1: filter, narrow (overlaps 0)
      @event_router.add_rule('app.web.access', @output)         # 2: exact output
      @event_router.add_rule('app.**', @wide_output)            # 3: wide output (shadowed for app.web.*)
      @event_router.add_rule('**', @catch_all_output)           # 4: catch-all fallback
    end

    test 'returns the filters and the first matching match in declaration order' do
      trace = @inspector.trace('app.web.access')

      assert_equal 3, trace.matched.size
      assert_equal [0, 1, 2], trace.matched.map(&:rule_index)
      assert_equal [:filter, :filter, :output], trace.matched.map(&:kind)
      assert_equal [@filter_a, @filter_b], trace.filter_plugins
      assert_same @output, trace.output_plugin
      assert_equal :first_matching_match, trace.terminal_reason
      assert_same @output, trace.terminal_rule.collector
    end

    test 'records why overlapping later declarations are skipped' do
      trace = @inspector.trace('app.web.access')

      skipped = trace.skipped
      assert_equal [3, 4], skipped.map(&:rule_index)
      assert_equal :not_evaluated_terminal_already_selected, skipped[0].reason
      assert_equal :not_evaluated_terminal_already_selected, skipped[1].reason
    end

    test 'wide match catches a tag that the narrow exact match does not cover' do
      trace = @inspector.trace('app.worker.jobs')

      # only the wide filter (rule 0) matches; the narrow one (rule 1) does not
      assert_equal [:filter, :output], trace.matched.map(&:kind)
      assert_equal [@filter_a], trace.filter_plugins
      assert_same @wide_output, trace.output_plugin
      assert_equal :first_matching_match, trace.terminal_reason

      skipped = trace.skipped
      narrow_filter = skipped.find { |entry| entry.rule_index == 1 }
      assert_equal :pattern_does_not_match, narrow_filter.reason
      assert_empty narrow_filter.matched_patterns
      exact = skipped.find { |entry| entry.rule_index == 2 }
      assert_equal :pattern_does_not_match, exact.reason
    end

    test 'catch-all is the terminal for unrelated tags and overlapping rules show reasons' do
      trace = @inspector.trace('system.metrics')

      assert_empty trace.matched.select { |entry| entry.rule_index < 4 }
      assert_same @catch_all_output, trace.output_plugin
      # the explicit <match **> declaration matches, so this is a normal terminal,
      # not a fallback to the router's internal default collector
      assert_equal :first_matching_match, trace.terminal_reason

      reasons = trace.skipped.map(&:reason)
      assert_equal [:pattern_does_not_match] * 4, reasons
      catch_all = trace.skipped.find { |entry| entry.rule_index == 4 }
      assert_nil catch_all
      # the catch-all is selected, so it is in matched, not skipped
      selected_catch_all = trace.matched.find { |entry| entry.rule_index == 4 }
      assert_not_nil selected_catch_all
      assert_equal :first_matching_match, selected_catch_all.reason
    end

    test 'trace never mutates the match cache' do
      assert_equal 0, @event_router.instance_variable_get(:@match_cache).size
      @inspector.trace('app.web.access')
      @inspector.trace('app.worker.jobs')
      assert_equal 0, @event_router.instance_variable_get(:@match_cache).size
    end

    test 'trace never calls plugin emit/filter methods' do
      assert_rr do
        [@filter_a, @filter_b].each do |filter|
          stub(filter).filter_stream.never
          stub(filter).filter.never
        end
        [@output, @wide_output, @catch_all_output, @default_collector].each do |out|
          stub(out).emit_events.never
        end
        @inspector.trace('app.web.access')
      end
      assert_equal 0, @filter_a.num
      assert_equal 0, @filter_b.num
    end
  end

  sub_test_case 'space-separated (Or) patterns' do
    setup do
      @event_router.add_rule('a b c', @output)
    end

    test 'reports which alternative of the rule actually matched' do
      trace = @inspector.trace('b')
      entry = trace.matched.first
      assert_equal ['b'], entry.matched_patterns
      assert_equal 'a b c', entry.rule.pattern_str
      assert_same @output, trace.output_plugin
    end

    test 'reports multiple matching alternatives' do
      # 'a.** b' with tag 'a.x' matches only a.**; build a truly overlapping rule
      @event_router = EventRouter.new(@default_collector, @emit_handler)
      @inspector = RouteInspector.new(@event_router)
      @event_router.add_rule('a.** a.x', @other_output)
      trace = @inspector.trace('a.x')
      assert_equal ['a.**', 'a.x'], trace.matched.first.matched_patterns
    end

    test 'unmatched tag falls back to the default collector' do
      trace = @inspector.trace('z')
      assert_empty trace.matched
      assert_same @default_collector, trace.output_plugin
      assert_equal :default_collector_no_match, trace.terminal_reason
    end
  end

  sub_test_case 'filters without a following match' do
    setup do
      @event_router.add_rule('app.**', @filter_a)
      @event_router.add_rule('app.**', @filter_b)
    end

    test 'chains all matching filters and ends on the default collector' do
      trace = @inspector.trace('app.web')
      assert_equal [@filter_a, @filter_b], trace.filter_plugins
      assert_same @default_collector, trace.output_plugin
      assert_equal :default_collector_after_filters, trace.terminal_reason
      assert_nil trace.terminal_rule
    end

    test 'unmatched tag does not select the filters' do
      trace = @inspector.trace('db.x')
      assert_empty trace.filter_plugins
      assert_equal [:pattern_does_not_match, :pattern_does_not_match], trace.skipped.map(&:reason)
    end
  end

  sub_test_case 'cross-check against the live match cache' do
    setup do
      @event_router.add_rule('app.**', @filter_a)
      @event_router.add_rule('app.**', @output)
      @event_router.add_rule('**', @catch_all_output)
    end

    test 'not cached before any emit and tracing does not populate the cache' do
      verification = @inspector.verify('app.web')
      assert_equal :not_cached, verification.status
      assert_equal false, verification.cached?
      assert_nil verification.cached_collector
      assert_equal 0, @event_router.instance_variable_get(:@match_cache).size
    end

    test 'consistent after a real emit built the cache entry' do
      @event_router.emit('app.web', Engine.now, 'k' => 'v')
      verification = @inspector.verify('app.web')
      assert_equal :consistent, verification.status
      assert verification.cached?
      assert verification.consistent?

      cached = verification.cached_collector
      assert_kind_of EventRouter::Pipeline, cached
      assert_equal [@filter_a], cached.filters
      assert_same @output, cached.output
    end

    test 'consistent for a direct-output route (no filters)' do
      @event_router.emit('sys.x', Engine.now, 'k' => 'v')
      verification = @inspector.verify('sys.x')
      assert_equal :consistent, verification.status
      assert_same @catch_all_output, verification.cached_collector
    end

    test 'mismatch when declarations change after the cache was populated' do
      @event_router.emit('app.web', Engine.now, 'k' => 'v')
      assert_equal :consistent, @inspector.verify('app.web').status

      # Simulate a config reload: replace the registered rules with a different
      # chain while the stale cache entry stays in place.
      new_filter = FluentTestFilter.new('__new__')
      new_router = EventRouter.new(@default_collector, @emit_handler)
      new_router.add_rule('app.**', new_filter)
      new_router.add_rule('app.**', @other_output)
      stale_cache = @event_router.instance_variable_get(:@match_cache)
      new_router.instance_variable_set(:@match_cache, stale_cache)

      verification = RouteInspector.new(new_router).verify('app.web')
      assert_equal :mismatch, verification.status
      assert_equal false, verification.consistent?
      assert_same @output, verification.cached_collector.output
      assert_same @other_output, verification.trace.output_plugin
      assert_equal [new_filter], verification.trace.filter_plugins
    end

    test 'mismatch when a cached direct output is replaced' do
      @event_router.emit('sys.x', Engine.now, 'k' => 'v')

      new_router = EventRouter.new(@default_collector, @emit_handler)
      new_router.add_rule('**', @other_output)
      stale_cache = @event_router.instance_variable_get(:@match_cache)
      new_router.instance_variable_set(:@match_cache, stale_cache)

      verification = RouteInspector.new(new_router).verify('sys.x')
      assert_equal :mismatch, verification.status
      assert_same @catch_all_output, verification.cached_collector
      assert_same @other_output, verification.trace.output_plugin
    end
  end

  sub_test_case 'parity with EventRouter#find across many tags' do
    setup do
      @event_router.add_rule('app.web.**', @filter_a)
      @event_router.add_rule('app.**', @filter_b)
      @event_router.add_rule('app.web.access', @output)
      @event_router.add_rule('app.**', @wide_output)
      @event_router.add_rule('**', @catch_all_output)
    end

    test 'trace chain equals the collector produced by find for every tag' do
      tags = %w[
        app.web.access app.web.detail app.web app.worker app.worker.jobs
        system.metrics db.x app a b
      ]

      tags.each do |tag|
        trace = @inspector.trace(tag)
        found = @event_router.send(:find, tag) || @default_collector
        expected = found.is_a?(EventRouter::Pipeline) ? found.output : found

        assert_same expected, trace.output_plugin, "terminal output mismatch for #{tag}"

        if found.is_a?(EventRouter::Pipeline)
          assert_equal found.filters, trace.filter_plugins, "filter chain mismatch for #{tag}"
        else
          assert_empty trace.filter_plugins, "unexpected filters for #{tag}"
        end
      end
    end
  end

  sub_test_case 'rendering' do
    setup do
      @event_router.add_rule('app.web.**', @filter_a)
      @event_router.add_rule('app.**', @output)
      @event_router.add_rule('**', @catch_all_output)
    end

    test 'render shows selected declarations, reasons and the terminal rule' do
      text = @inspector.render('app.web.access')
      assert_match %r{route for tag: app\.web\.access}, text
      assert_match %r{\* <filter app\.web\.\*\*>}, text
      assert_match %r{\* <match app\.\*\*>}, text
      assert_match %r{- <match \*\*> .*\[not_evaluated_terminal_already_selected\]}, text
      assert_match %r{=> terminated by <match app\.\*\*>}, text
    end

    test 'render_verification reports an uncached tag plus the trace' do
      text = @inspector.render_verification('app.web.access')
      assert_match %r{cache: not_cached}, text
      assert_match %r{route for tag: app\.web\.access}, text
    end

    test 'render_verification describes a cached pipeline by its inner chain' do
      @event_router.emit('app.web.access', Engine.now, 'k' => 'v')
      text = @inspector.render_verification('app.web.access')
      assert_match %r{cached collector: pipeline\[test_filter\([^)]*\) -> test_out\([^)]*\)\]}, text
    end
  end
end
