require_relative 'helper'
require 'fluent/root_agent'
require 'fluent/route_inspector'
require_relative 'test_plugin_classes'

# End-to-end checks against a real RootAgent built from a configuration with
# overlapping matches: the declaration-based trace must describe exactly the
# same chain as the in-memory collector that EventRouter actually returns.
class RouteInspectorAgentTest < ::Test::Unit::TestCase
  include Fluent
  include FluentTest

  setup do
    @root_agent = RootAgent.new(log: $log)
    stub(Engine).root_agent { @root_agent }
  end

  CONFIG = <<~CONF.freeze
    <source>
      @type test_in
    </source>
    <filter app.**>
      @type test_filter
      @id filter_wide
    </filter>
    <filter app.web.**>
      @type test_compat_filter
      @id filter_narrow
    </filter>
    <match app.web.access>
      @type test_out
      @id out_exact
    </match>
    <match app.**>
      @type test_out
      @id out_wide
    </match>
    <match **>
      @type test_out
      @id out_catch_all
    </match>
  CONF

  def configured_router
    conf = Config.parse(CONFIG, '(test)', '(test_dir)', true)
    @root_agent.configure(conf)
    @root_agent.event_router
  end

  def assert_same_chain_as_memory_route(router, tag)
    inspector = RouteInspector.new(router)
    trace = inspector.trace(tag)

    # Populate (or fetch) the real in-memory route exactly like emit_stream does.
    cached_collector = router.match(tag)
    verification = inspector.verify(tag)

    assert_equal :consistent, verification.status,
                 "traced route and cached route disagree for #{tag}"
    assert_same cached_collector, verification.cached_collector

    case cached_collector
    when EventRouter::Pipeline
      assert_equal cached_collector.filters, trace.filter_plugins
      assert_same cached_collector.output, trace.output_plugin
    else
      assert_empty trace.filter_plugins
      assert_same cached_collector, trace.output_plugin
    end
  end

  test 'trace follows the first matching match after the matching filters' do
    router = configured_router
    trace = RouteInspector.new(router).trace('app.web.access')

    kinds = trace.matched.map(&:kind)
    assert_equal [:filter, :filter, :output], kinds
    assert_equal ['app.**', 'app.web.**', 'app.web.access'],
                 trace.matched.map { |entry| entry.rule.pattern_str }
    assert_equal :first_matching_match, trace.terminal_reason
    assert_equal 'out_exact', trace.output_plugin.plugin_id

    # later overlapping declarations are never evaluated once a match terminates lookup
    later = trace.skipped.map(&:rule_index)
    assert_equal [3, 4], later
    assert(trace.skipped.all? { |entry| entry.reason == :not_evaluated_terminal_already_selected })
  end

  test 'wide output wins for a tag the exact match does not cover' do
    router = configured_router
    trace = RouteInspector.new(router).trace('app.worker.jobs')

    assert_equal ['filter_wide'], trace.filter_plugins.map(&:plugin_id)
    assert_equal 'out_wide', trace.output_plugin.plugin_id

    skipped_patterns = trace.skipped.map { |entry| entry.rule.pattern_str }
    assert_include skipped_patterns, 'app.web.**'
    assert_include skipped_patterns, 'app.web.access'
  end

  test 'catch-all output handles unrelated tags' do
    router = configured_router
    trace = RouteInspector.new(router).trace('system.health')

    assert_empty trace.filter_plugins
    assert_equal 'out_catch_all', trace.output_plugin.plugin_id
    assert_equal :first_matching_match, trace.terminal_reason
  end

  test 'trace and the in-memory route agree for every representative tag' do
    router = configured_router
    %w[
      app.web.access app.web.detail app.web app.worker.jobs
      app system.health db.query a
    ].each do |tag|
      assert_same_chain_as_memory_route(router, tag)
    end
  end

  test 'inspection does not emit through any plugin or alter the cache' do
    router = configured_router
    cache = router.instance_variable_get(:@match_cache)
    inspector = RouteInspector.new(router)

    assert_equal 0, cache.size
    inspector.trace('app.web.access')
    inspector.verify('app.web.access')
    inspector.render('app.web.access')
    assert_equal 0, cache.size

    # outputs registered from the configuration must not have received events
    @root_agent.outputs.each do |output|
      assert_empty output.events
    end
  end

  test 'detects a stale cache entry after declarations are replaced' do
    router = configured_router
    inspector = RouteInspector.new(router)

    # populate the cache through the real lookup path
    router.match('app.web.access')
    assert_equal :consistent, inspector.verify('app.web.access').status

    # build a second router whose declarations produce a different chain,
    # then transplant the first router's populated cache (config-reload scenario)
    other_agent = RootAgent.new(log: $log)
    other_conf = Config.parse(<<~CONF, '(test)', '(test_dir)', true)
      <match app.**>
        @type test_out
        @id out_reloaded
      </match>
    CONF
    other_agent.configure(other_conf)
    other_router = other_agent.event_router
    other_router.instance_variable_set(:@match_cache, router.instance_variable_get(:@match_cache))

    verification = RouteInspector.new(other_router).verify('app.web.access')
    assert_equal :mismatch, verification.status
    assert_equal 'out_reloaded', verification.trace.output_plugin.plugin_id
  end
end
