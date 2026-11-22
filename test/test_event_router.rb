require_relative 'helper'
require 'fluent/event_router'
require_relative 'test_plugin_classes'

class EventRouterTest < ::Test::Unit::TestCase
  include Fluent
  include FluentTest

  teardown do
    @output = nil
    @filter = nil
    @compat_filter = nil
    @error_output = nil
    @emit_handler = nil
    @default_collector = nil
  end

  def output
    @output ||= FluentTestOutput.new
  end

  def filter
    @filter ||= FluentTestFilter.new
  end

  def compat_filter
    @compat_filter ||= FluentCompatTestFilter.new
  end

  def error_output
    @error_output ||= FluentTestErrorOutput.new
  end

  def emit_handler
    @emit_handler ||= TestEmitErrorHandler.new
  end

  def default_collector
    @default_collector ||= FluentTestOutput.new
  end

  def event(record, time = Engine.now)
    OneEventStream.new(time, record)
  end

  DEFAULT_EVENT_NUM = 5

  def events(num = DEFAULT_EVENT_NUM)
    es = MultiEventStream.new
    num.times { |i|
      es.add(Engine.now, 'key' => "value#{i}")
    }
    es
  end

  sub_test_case EventRouter::MatchCache do
    setup do
      @match_cache = EventRouter::MatchCache.new
    end

    test "call block when non-cached key" do
      assert_raise(RuntimeError.new('Test!')) {
        @match_cache.get('test') { raise 'Test!' }
      }
    end

    test "don't call block when cached key" do
      @match_cache.get('test') { "I'm cached" }
      assert_nothing_raised {
        @match_cache.get('test') { raise 'Test!' }
      }
      assert_equal "I'm cached", @match_cache.get('test') { raise 'Test!' }
    end

    test "call block when keys are expired" do
      cache_size = EventRouter::MatchCache::MATCH_CACHE_SIZE
      cache_size.times { |i|
        @match_cache.get("test#{i}") { "I'm cached #{i}" }
      }
      assert_nothing_raised {
        cache_size.times { |i|
          @match_cache.get("test#{i}") { raise "Why called?" }
        }
      }
      # expire old keys
      cache_size.times { |i|
        @match_cache.get("new_test#{i}") { "I'm young #{i}" }
      }
      num_called = 0
      cache_size.times { |i|
        @match_cache.get("test#{i}") { num_called += 1 }
      }
      assert_equal cache_size, num_called
    end
  end

  sub_test_case EventRouter::Pipeline do
    setup do
      @pipeline = EventRouter::Pipeline.new
      @es = event('key' => 'value')
    end

    test 'set one output' do
      @pipeline.set_output(output)
      @pipeline.emit_events('test', @es)
      assert_equal 1, output.events.size
      assert_equal 'value', output.events['test'].first['key']
    end

    sub_test_case 'with filter' do
      setup do
        @pipeline.set_output(output)
      end

      data('Filter plugin' => 'filter',
           'Compat::Filter plugin' => 'compat_filter')
      test 'set one filter' do |filter_type|
        @pipeline.add_filter(filter_type == 'filter' ? filter : compat_filter)
        @pipeline.emit_events('test', @es)
        assert_equal 1, output.events.size
        assert_equal 'value', output.events['test'].first['key']
        assert_equal 0, output.events['test'].first['__test__']
      end

      data('Filter plugin' => 'filter',
           'Compat::Filter plugin' => 'compat_filter')
      test 'set one filter with multi events' do |filter_type|
        @pipeline.add_filter(filter_type == 'filter' ? filter : compat_filter)
        @pipeline.emit_events('test', events)
        assert_equal 1, output.events.size
        assert_equal 5, output.events['test'].size
        DEFAULT_EVENT_NUM.times { |i|
          assert_equal "value#{i}", output.events['test'][i]['key']
          assert_equal i, output.events['test'][i]['__test__']
        }
      end
    end
  end

  sub_test_case 'filter chain fault tolerance protocol' do
    # Filter that always succeeds: records how many times each record was
    # seen and tags the record with a stage marker.
    def passing_filter(marker)
      Class.new(FluentTestFilter) do
        define_method(:initialize) do |*args|
          super(*args)
          @marker = marker
          @seen = Hash.new(0)
        end

        attr_reader :seen

        define_method(:filter) do |tag, time, record|
          key = record['id']
          @seen[key] += 1
          record['stages'] = (record['stages'] || []) + [@marker]
          record
        end
      end.new(marker)
    end

    # Filter that intentionally drops a specific record (returns nil),
    # everything else passes through untouched.
    def dropping_filter(dropped_id)
      Class.new(FluentTestFilter) do
        define_method(:initialize) do |id|
          super()
          @dropped_id = id
          @dropped = []
        end

        attr_reader :dropped

        define_method(:filter) do |tag, time, record|
          if record['id'] == @dropped_id
            @dropped << record
            nil
          else
            record['stages'] = (record['stages'] || []) + ['b']
            record
          end
        end
      end.new(dropped_id)
    end

    # Filter that mutates the record and THEN raises for a specific id.
    # The error route must receive the snapshot taken *before* this filter
    # ran, never the partially mutated record.
    def failing_filter(failing_id, error_message)
      Class.new(FluentTestFilter) do
        define_method(:initialize) do |id, message|
          super()
          @failing_id = id
          @message = message
          @attempts = Hash.new(0)
        end

        attr_reader :attempts

        define_method(:filter) do |tag, time, record|
          @attempts[record['id']] += 1
          if record['id'] == @failing_id
            record['partial_mutation'] = true
            raise RuntimeError, @message
          end
          record['stages'] = (record['stages'] || []) + ['c']
          record
        end
      end.new(failing_id, error_message)
    end

    # Filter overriding #filter_stream, which forces the pipeline to disable
    # optimization and use the plain reduce path. It delegates the per-record
    # decision to an inner failing filter and applies the same filter_one
    # fault-tolerance protocol as the base class.
    def failing_stream_filter(failing_id, error_message)
      inner = failing_filter(failing_id, error_message)
      Class.new(FluentTestFilter) do
        define_method(:initialize) do |delegate|
          super()
          @delegate = delegate
        end

        attr_reader :delegate

        define_method(:filter_stream) do |tag, es|
          new_es = MultiEventStream.new
          es.each do |time, record|
            result = filter_one(tag, time, record)
            new_es.add(result.time, result.record) if result.passed?
          end
          new_es
        end

        define_method(:filter) do |tag, time, record|
          @delegate.send(:filter, tag, time, record)
        end

        define_method(:attempts) do
          @delegate.attempts
        end
      end.new(inner)
    end

    # A filter_stream filter that drops a given record, again forcing the
    # non-optimized reduce path for the whole chain.
    def dropping_stream_filter(dropped_id)
      inner = dropping_filter(dropped_id)
      Class.new(FluentTestFilter) do
        define_method(:initialize) do |delegate|
          super()
          @delegate = delegate
        end

        attr_reader :delegate

        define_method(:filter_stream) do |tag, es|
          new_es = MultiEventStream.new
          es.each do |time, record|
            result = filter_one(tag, time, record)
            new_es.add(result.time, result.record) if result.passed?
          end
          new_es
        end

        define_method(:filter) do |tag, time, record|
          @delegate.send(:filter, tag, time, record)
        end
      end.new(inner)
    end

    # Error handler that records every record sent to the error route.
    def capturing_error_handler
      Class.new do
        attr_reader :error_events

        def initialize
          @error_events = []
        end

        def handle_emit_error(tag, time, record, error)
          @error_events << { tag: tag, time: time, record: record, error: error }
        end

        def handle_emits_error(tag, es, error)
          es.each { |time, record| handle_emit_error(tag, time, record, error) }
        end

        def emit_error_event(tag, time, record, error)
          handle_emit_error(tag, time, record, error)
        end
      end.new
    end

    def batch_events
      es = MultiEventStream.new
      [1, 2, 3].each do |id|
        es.add(Engine.now + id, 'id' => id, 'stages' => [])
      end
      es
    end

    # Build the same logical three-stage chain for both paths:
    #   stage 1: passing (marks 'a')
    #   stage 2: drops record 2 (marks 'b' otherwise)
    #   stage 3: fails record 3 after a partial mutation (marks 'c' otherwise)
    def build_pipeline(stream_chain:)
      pipeline = EventRouter::Pipeline.new
      pipeline.add_filter(passing_filter('a'))
      if stream_chain
        pipeline.add_filter(dropping_stream_filter(2))
        pipeline.add_filter(failing_stream_filter(3, 'boom on stream chain'))
      else
        pipeline.add_filter(dropping_filter(2))
        pipeline.add_filter(failing_filter(3, 'boom on optimized chain'))
      end
      pipeline.set_output(FluentTestOutput.new)
      pipeline
    end

    def attach_handler(pipeline, handler)
      pipeline.instance_variable_get(:@optimizer).instance_variable_get(:@filters).each do |f|
        f.router = handler
        if f.respond_to?(:delegate) && f.delegate
          f.delegate.router = handler
        end
      end
    end

    def expected_business_records
      [
        { 'id' => 1, 'stages' => ['a', 'b', 'c'] },
      ]
    end

    test 'optimized path: marks passed / dropped / error-routed records correctly' do
      pipeline = build_pipeline(stream_chain: false)
      handler = capturing_error_handler
      attach_handler(pipeline, handler)

      pipeline.emit_events('test.tag', batch_events)
      output = pipeline.instance_variable_get(:@output)

      # only the fully-passed record reaches the business output
      assert_equal expected_business_records, output.events['test.tag']

      # exactly one error event, carrying the pre-failure snapshot
      assert_equal 1, handler.error_events.size
      err = handler.error_events.first
      assert_equal 3, err[:record]['id']
      assert_not_include err[:record].keys, 'partial_mutation'
      assert_equal ['a', 'b'], err[:record]['stages']
      assert_equal RuntimeError, err[:error].class
    end

    test 'non-optimized (filter_stream) path: identical business output and error route' do
      pipeline = build_pipeline(stream_chain: true)
      handler = capturing_error_handler
      attach_handler(pipeline, handler)

      pipeline.emit_events('test.tag', batch_events)
      output = pipeline.instance_variable_get(:@output)

      assert_equal expected_business_records, output.events['test.tag']

      assert_equal 1, handler.error_events.size
      err = handler.error_events.first
      assert_equal 3, err[:record]['id']
      assert_not_include err[:record].keys, 'partial_mutation'
      assert_equal ['a', 'b'], err[:record]['stages']
      assert_equal RuntimeError, err[:error].class
    end

    test 'both paths deliver the same record dispositions for the same batch' do
      optimized = build_pipeline(stream_chain: false)
      streamed = build_pipeline(stream_chain: true)
      h_opt = capturing_error_handler
      h_str = capturing_error_handler
      attach_handler(optimized, h_opt)
      attach_handler(streamed, h_str)

      optimized.emit_events('test.tag', batch_events)
      streamed.emit_events('test.tag', batch_events)

      out_opt = optimized.instance_variable_get(:@output).events['test.tag']
      out_str = streamed.instance_variable_get(:@output).events['test.tag']
      assert_equal out_opt, out_str

      err_opt = h_opt.error_events.map { |e| e[:record] }
      err_str = h_str.error_events.map { |e| e[:record] }
      assert_equal err_opt, err_str
    end

    test 'retry does not re-send records already delivered to the error route' do
      pipeline = build_pipeline(stream_chain: false)
      handler = capturing_error_handler
      attach_handler(pipeline, handler)

      es = batch_events
      pipeline.emit_events('test.tag', es)
      first_output = pipeline.instance_variable_get(:@output).events['test.tag'].size
      first_errors = handler.error_events.size

      # simulate an upstream retry reprocessing the exact same batch
      pipeline.emit_events('test.tag', es)
      second_output = pipeline.instance_variable_get(:@output).events['test.tag'].size
      second_errors = handler.error_events.size

      # the failing record must not be delivered to @ERROR again
      assert_equal first_errors, second_errors
      # the still-valid record is naturally reprocessed, but no extra
      # error-routed duplicates appear
      assert_equal first_output * 2, second_output
    end

    test 'retry is also idempotent on the non-optimized filter_stream path' do
      pipeline = build_pipeline(stream_chain: true)
      handler = capturing_error_handler
      attach_handler(pipeline, handler)

      es = batch_events
      pipeline.emit_events('test.tag', es)
      first_errors = handler.error_events.size

      pipeline.emit_events('test.tag', es)
      second_errors = handler.error_events.size

      assert_equal first_errors, second_errors
    end

    test 'FilterResult exposes the three dispositions explicitly' do
      passing = passing_filter('x')
      passing.router = capturing_error_handler
      r_pass = passing.filter_one('t', Engine.now, 'id' => 1)
      assert r_pass.passed?
      assert !r_pass.dropped?
      assert !r_pass.routed_to_error?

      dropping = dropping_filter(9)
      dropping.router = capturing_error_handler
      r_drop = dropping.filter_one('t', Engine.now, 'id' => 9)
      assert r_drop.dropped?
      assert !r_drop.passed?

      failing = failing_filter(9, 'boom')
      failing.router = capturing_error_handler
      r_err = failing.filter_one('t', Engine.now, 'id' => 9)
      assert r_err.routed_to_error?
      assert !r_err.passed?
      assert_instance_of RuntimeError, r_err.error
    end

    # A filter built on #filter_with_time instead of #filter must follow the
    # exact same three-state protocol (this is the other optimizer branch).
    test 'protocol also applies to filter_with_time based plugins' do
      fwt = Class.new(FluentTestFilter) do
        undef_method :filter

        def filter_with_time(tag, time, record)
          if record['id'] == 2
            record['partial_mutation'] = true
            raise RuntimeError, 'fwt boom'
          elsif record['id'] == 3
            # explicit drop via nil time/record
            return [nil, nil]
          end
          [time, record]
        end
      end.new
      handler = capturing_error_handler
      fwt.router = handler

      es = MultiEventStream.new
      es.add(10, 'id' => 1)
      es.add(20, 'id' => 2)
      es.add(30, 'id' => 3)

      out = fwt.filter_stream('wt', es)
      results = out.to_a

      # only id 1 is forwarded
      assert_equal 1, results.size
      assert_equal 10, results.first[0]
      assert_equal 1, results.first[1]['id']

      # id 2 reached the error route with its pristine pre-filter snapshot
      assert_equal 1, handler.error_events.size
      err = handler.error_events.first
      assert_equal 2, err[:record]['id']
      assert !err[:record].key?('partial_mutation')

      # retrying the whole stream does not duplicate the error event
      fwt.filter_stream('wt', es)
      assert_equal 1, handler.error_events.size
    end

    test 'filter_with_time result time is honored through the protocol' do
      fwt = Class.new(FluentTestFilter) do
        undef_method :filter

        def filter_with_time(tag, time, record)
          [time + 100, record]
        end
      end.new
      fwt.router = capturing_error_handler

      result = fwt.filter_one('wt', 10, 'id' => 1)
      assert result.passed?
      assert_equal 110, result.time
    end
  end

  sub_test_case EventRouter do
    teardown do
      @event_router = nil
    end

    def event_router
      @event_router ||= EventRouter.new(default_collector, emit_handler)
    end

    sub_test_case 'default collector' do
      test 'call default collector when no output' do
        assert_rr do
          mock(default_collector).emit_events('test', is_a(OneEventStream))
          event_router.emit('test', Engine.now, 'k' => 'v')
        end
      end

      test "call default collector when only filter" do
        event_router.add_rule('test', filter)
        assert_rr do
          # After apply Filter, EventStream becomes MultiEventStream by default
          mock(default_collector).emit_events('test', is_a(MultiEventStream))
          event_router.emit('test', Engine.now, 'k' => 'v')
        end
        assert_equal 1, filter.num
      end

      test "call default collector when no matched with output" do
        event_router.add_rule('test', output)
        assert_rr do
          mock(default_collector).emit_events('dummy', is_a(OneEventStream))
          event_router.emit('dummy', Engine.now, 'k' => 'v')
        end
      end

      test "don't call default collector when tag matched" do
        event_router.add_rule('test', output)
        assert_rr do
          mock(default_collector).emit_events('test', is_a(OneEventStream)).never
          event_router.emit('test', Engine.now, 'k' => 'v')
        end
        # check emit handler doesn't catch rr error
        assert_empty emit_handler.events
      end
    end

    sub_test_case 'filter' do
      test 'filter should be called when tag matched' do
        filter = Class.new(FluentTestFilter) { |x|
          def filter_stream(_tag, es); end
        }.new

        event_router.add_rule('test', filter)

        assert_rr do
          mock(filter).filter_stream('test', is_a(OneEventStream)) { events }
          event_router.emit('test', Engine.now, 'k' => 'v')
        end
      end

      test 'filter should not be called when tag mismatched' do
        event_router.add_rule('test', filter)

        assert_rr do
          mock(filter).filter_stream('test', is_a(OneEventStream)).never
          event_router.emit('foo', Engine.now, 'k' => 'v')
        end
      end

      test 'filter changes records' do
        event_router.add_rule('test', filter)
        event_router.add_rule('test', output)
        event_router.emit('test', Engine.now, 'k' => 'v')

        assert_equal 1, filter.num
        assert_equal 1, output.events['test'].size
        assert_equal 0, output.events['test'].first['__test__']
        assert_equal 'v', output.events['test'].first['k']
      end

      test 'filter can be chained' do
        other_filter = FluentTestFilter.new('__hoge__')
        event_router.add_rule('test', filter)
        event_router.add_rule('test', other_filter)
        event_router.add_rule('test', output)
        event_router.emit('test', Engine.now, 'k' => 'v')

        assert_equal 1, filter.num
        assert_equal 1, other_filter.num
        assert_equal 1, output.events['test'].size
        assert_equal 0, output.events['test'].first['__test__']
        assert_equal 0, output.events['test'].first['__hoge__']
        assert_equal 'v', output.events['test'].first['k']
      end
    end

    sub_test_case 'optimized filter' do
      setup do
        @record = { 'k' => 'v' }
        @now = Engine.now
      end

      test 'call optimized filter when the filter plugin implements #filter without #filter_stream' do
        event_router.add_rule('test', filter)

        assert_rr do
          mock(filter).filter('test', @now, @record) { @record }
          event_router.emit('test', @now, @record)
        end
      end

      test 'call optimized filter when the filter plugin implements #filter_with_time without #filter_stream' do
        filter = Class.new(FluentTestFilter) {
          undef_method :filter
          def filter_with_time(tag, time, record); end
        }.new

        event_router.add_rule('test', filter)

        assert_rr do
          mock(filter).filter_with_time('test', @now, @record) { [time, @record] }
          event_router.emit('test', @now, @record)
        end
      end

      test "don't call optimized filter when filter plugins implement #filter_stream" do
        filter = Class.new(FluentTestFilter) {
          undef_method :filter
          def filter_stream(tag, time, record); end
        }.new

        event_router.add_rule('test', filter)

        assert_rr do
          mock(filter).filter_stream('test', is_a(OneEventStream)) { OneEventStream.new(@now, @record) }
          event_router.emit('test', @now, @record)
        end
      end

      test 'call optimized filter when filter plugins have #filter_with_time instead of #filter' do
        filter_with_time = Class.new(FluentTestFilter) {
          undef_method :filter
          def filter_with_time(tag, time, record); end
        }.new

        event_router.add_rule('test', filter_with_time)
        event_router.add_rule('test', filter)

        assert_rr do
          mock(filter_with_time).filter_with_time('test', @now, @record) { [@now + 1, @record] }
          mock(filter).filter('test', @now + 1, @record) { @record }
          event_router.emit('test', @now, @record)
        end
      end

      test "don't call optimized filter even if just a filter of some filters implements #filter_stream method" do
        filter_stream = Class.new(FluentTestFilter) {
          def filter_stream(tag, record); end
        }.new

        filter_with_time = Class.new(FluentTestFilter) {
          undef_method :filter
          def filter_with_time(tag, time, record); end
        }.new

        filters = [filter_stream, filter_with_time, filter]
        filters.each { |f| event_router.add_rule('test', f) }

        e = OneEventStream.new(@now, @record)
        assert_rr do
          mock($log).info("disable filter chain optimization because #{[filter_stream].map(&:class)} uses `#filter_stream` method.")
          mock(filter_stream).filter_stream('test', is_a(OneEventStream)) { e }
          mock(filter).filter_stream('test', is_a(OneEventStream)) { e }
          mock(filter_with_time).filter_stream('test', is_a(OneEventStream)) { e }
          event_router.emit('test', @now, @record)
        end
      end
    end

    sub_test_case 'emit_error_handler' do
      test 'call handle_emits_error when emit failed' do
        event_router.add_rule('test', error_output)

        event_router.emit('test', Engine.now, 'k' => 'v')
        assert_rr do
          mock(emit_handler).handle_emits_error('test', is_a(OneEventStream), is_a(RuntimeError))
          event_router.emit('test', Engine.now, 'k' => 'v')
        end
      end

      test 'can pass records modified by filters to handle_emits_error' do
        filter = Class.new(FluentTestFilter) {
          def filter_stream(_tag, es); end
        }.new
        event_router.add_rule('test', filter)
        event_router.add_rule('test', error_output)

        time = Engine.now
        modified_es = OneEventStream.new(time, 'modified_label' => 'modified_value')

        assert_rr do
          stub(filter).filter_stream { modified_es }
          mock(emit_handler).handle_emits_error('test', modified_es, is_a(RuntimeError))
          event_router.emit('test', time, 'pre_label' => 'pre_value')
        end
      end
    end
  end
end
