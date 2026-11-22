require_relative '../helper'
require 'fluent/test/driver/multi_output'
require 'fluent/plugin/out_copy'
require 'fluent/event'
require 'flexmock/test_unit'

module FluentPluginCopyOutputTest
  # In-memory store used to compare what each <store> actually received.
  class RecordingOutput < Fluent::Plugin::Output
    Fluent::Plugin.register_output('copy_recording', self)

    config_param :name, :string
    config_param :limit, :integer, default: nil

    attr_reader :streams, :events

    def initialize
      super
      @streams = []
      @events = []
    end

    def prefer_buffered_processing
      false
    end

    def process(tag, es)
      @streams << es
      count = 0
      es.each do |time, record|
        @events << [time, record]
        count += 1
        break if @limit && count >= @limit
      end
    end

    def write(chunk); end
  end

  # In-memory *buffered* store, to exercise the chunk/flush path with copied streams.
  class MemoryBufferedRecordingOutput < Fluent::Plugin::Output
    Fluent::Plugin.register_output('copy_recording_memory', self)

    config_param :name, :string
    # Optional in-place modification, applied to the records this store got.
    config_param :modify, :bool, default: false

    attr_reader :events

    def initialize
      super
      @events = []
    end

    def prefer_buffered_processing
      true
    end

    def write(chunk)
      chunk.each do |time, record|
        if modify
          record["seen_by"] = name
          record["nest"]["k"] = name
        end
        @events << [time, record]
      end
    end
  end

  # A streaming (non-repeatable by default) input that records how often it is walked.
  class ProbeEventStream < Fluent::EventStream
    attr_reader :iterations

    def initialize(entries, repeatable: false)
      @entries = entries
      @repeatable = repeatable
      @iterations = 0
    end

    def repeatable?
      @repeatable
    end

    def size
      @entries.size
    end

    def empty?
      @entries.empty?
    end

    def dup
      Fluent::ArrayEventStream.new(@entries)
    end

    def slice(index, num)
      Fluent::ArrayEventStream.new(@entries.slice(index, num))
    end

    def each(unpacker: nil, &block)
      @iterations += 1
      @entries.each(&block)
      nil
    end
  end
end

class CopyOutputTest < Test::Unit::TestCase
  include FlexMock::TestCase

  class << self
    def startup
      $LOAD_PATH.unshift File.expand_path(File.join(File.dirname(__FILE__), '..', 'scripts'))
      require 'fluent/plugin/out_test'
      require 'fluent/plugin/out_test2'
    end

    def shutdown
      $LOAD_PATH.shift
    end
  end

  def setup
    Fluent::Test.setup
  end

  CONFIG = %[
    <store>
      @type test
      name c0
    </store>
    <store>
      @type test2
      name c1
    </store>
    <store>
      @type test
      name c2
    </store>
  ]

  def create_driver(conf = CONFIG)
    Fluent::Test::Driver::MultiOutput.new(Fluent::Plugin::CopyOutput).configure(conf)
  end

  def test_configure
    d = create_driver

    outputs = d.instance.outputs
    assert_equal 3, outputs.size
    assert_equal Fluent::Plugin::TestOutput, outputs[0].class
    assert_equal Fluent::Plugin::Test2Output, outputs[1].class
    assert_equal Fluent::Plugin::TestOutput, outputs[2].class
    assert_equal "c0", outputs[0].name
    assert_equal "c1", outputs[1].name
    assert_equal "c2", outputs[2].name
    assert_false d.instance.deep_copy
    assert_equal :no_copy, d.instance.copy_mode
  end

  ERRORNEOUS_IGNORE_IF_PREV_SUCCESS_CONFIG = %[
    <store ignore_if_prev_success ignore_error>
      @type test
      name c0
    </store>
    <store ignore_if_prev_success ignore_error>
      @type test
      name c1
    </store>
    <store ignore_if_prev_success>
      @type test
      name c2
    </store>
  ]
  def test_configure_with_errorneus_ignore_if_prev_success
    assert_raise(Fluent::ConfigError) do
      create_driver(ERRORNEOUS_IGNORE_IF_PREV_SUCCESS_CONFIG)
    end
  end

  ALL_IGNORE_ERROR_WITHOUT_IGNORE_IF_PREV_SUCCESS_CONFIG = %[
    @log_level info
    <store ignore_error>
      @type test
      name c0
    </store>
    <store ignore_error>
      @type test
      name c1
    </store>
    <store ignore_error>
      @type test
      name c2
    </store>
  ]
  def test_configure_all_ignore_errors_without_ignore_if_prev_success
    d = create_driver(ALL_IGNORE_ERROR_WITHOUT_IGNORE_IF_PREV_SUCCESS_CONFIG)
    expected = /ignore_errors are specified in all <store>, but ignore_if_prev_success is not specified./
    matches = d.logs.grep(expected)
    assert_equal(1, matches.length, "Logs do not contain '#{expected}' '#{d.logs}'")
  end

  def test_configure_with_deep_copy_and_use_shallow_copy_mode
    d = create_driver(%[
      deep_copy true
      <store>
        @type test
        name c0
      </store>
    ])

    outputs = d.instance.outputs
    assert_equal 1, outputs.size
    assert_equal Fluent::Plugin::TestOutput, outputs[0].class
    assert_equal "c0", outputs[0].name
    assert_true d.instance.deep_copy
    assert_equal :shallow, d.instance.copy_mode
  end

  def test_configure_store_copy_mode_overrides_plugin_copy_mode
    d = create_driver(%[
      copy_mode shallow
      <store>
        @type test
        name c0
      </store>
      <store>
        @type test
        name c1
        copy_mode deep
      </store>
      <store>
        @type test
        name c2
        copy_mode no_copy
      </store>
    ])

    assert_equal :shallow, d.instance.copy_mode
    assert_equal [nil, :deep, :no_copy], d.instance.stores.map(&:copy_mode)
    # plugin-wide copy_mode applies to stores that do not specify their own
    inherited_proc, deep_proc, no_copy_proc = d.instance.instance_variable_get(:@copy_procs)
    assert_not_nil inherited_proc
    assert_not_nil deep_proc
    assert_nil no_copy_proc
  end

  def test_configure_store_copy_mode_rejects_unknown_value
    assert_raise(Fluent::ConfigError) do
      create_driver(%[
        <store>
          @type test
          name c0
          copy_mode bogus
        </store>
      ])
    end
  end

  def test_deep_copy_and_copy_mode_conflict
    d = create_driver(%[
      deep_copy true
      copy_mode deep
      <store>
        @type test
        name c0
      </store>
    ])
    # `deep_copy` keeps winning over `copy_mode` for compatibility
    assert_equal :shallow, d.instance.copy_mode
  end

  def test_feed_events
    d = create_driver

    assert !d.instance.outputs[0].has_router?
    assert_not_nil d.instance.outputs[1].router
    assert !d.instance.outputs[2].has_router?

    time = event_time("2011-01-02 13:14:15 UTC")
    d.run(default_tag: 'test') do
      d.feed(time, {"a" => 1})
      d.feed(time, {"a" => 2})
    end

    d.instance.outputs.each {|o|
      assert_equal [ [time, {"a"=>1}], [time, {"a"=>2}] ], o.events
    }
  end

  def test_msgpack_unpacker_cache_bug_for_msgpack_event_stream
    d = create_driver

    time = event_time("2011-01-02 13:14:15 UTC")
    source = Fluent::ArrayEventStream.new([ [time, {"a" => 1}], [time, {"a" => 2}] ])
    es = Fluent::MessagePackEventStream.new(source.to_msgpack_stream)

    d.run(default_tag: 'test') do
      d.feed(es)
    end

    d.instance.outputs.each { |o|
      assert_equal [ [time, {"a"=>1}], [time, {"a"=>2}] ], o.events
    }
  end

  def create_event_test_driver(copy_mode = 'no_copy')
    config = %[
      copy_mode #{copy_mode}
      <store>
        @type test
        name output1
      </store>
      <store>
        @type test
        name output2
      </store>
    ]

    d = Fluent::Test::Driver::MultiOutput.new(Fluent::Plugin::CopyOutput).configure(config)
    d.instance.outputs[0].define_singleton_method(:process) do |tag, es|
      es.each do |time, record|
        record['foo'] = 'bar'
      end
      super(tag, es)
    end
    d
  end

  time = event_time("2013-05-26 06:37:22 UTC")
  gen_multi_es = Proc.new {
    es = Fluent::MultiEventStream.new
    es.add(time, {"a" => 1, "nest" => {'k' => 'v'}})
    es.add(time, {"b" => 1, "nest" => {'k' => 'v'}})
    es
  }

  data(
    "OneEventStream without copy" => ['no_copy', Fluent::OneEventStream.new(time, {"a" => 1, "nest" => {'k' => 'v'}})],
    "OneEventStream with shallow" => ['shallow', Fluent::OneEventStream.new(time, {"a" => 1, "nest" => {'k' => 'v'}})],
    "OneEventStream with marshal" => ['marshal', Fluent::OneEventStream.new(time, {"a" => 1, "nest" => {'k' => 'v'}})],
    "OneEventStream with deep"    => ['deep',    Fluent::OneEventStream.new(time, {"a" => 1, "nest" => {'k' => 'v'}})],
    "ArrayEventStream without copy" => ['no_copy', Fluent::ArrayEventStream.new([[time, {"a" => 1, "nest" => {'k' => 'v'}}], [time, {"b" => 2, "nest" => {'k' => 'v'}}]])],
    "ArrayEventStream with shallow" => ['shallow', Fluent::ArrayEventStream.new([[time, {"a" => 1, "nest" => {'k' => 'v'}}], [time, {"b" => 2, "nest" => {'k' => 'v'}}]])],
    "ArrayEventStream with marshal" => ['marshal', Fluent::ArrayEventStream.new([[time, {"a" => 1, "nest" => {'k' => 'v'}}], [time, {"b" => 2, "nest" => {'k' => 'v'}}]])],
    "ArrayEventStream with deep"    => ['deep',    Fluent::ArrayEventStream.new([[time, {"a" => 1, "nest" => {'k' => 'v'}}], [time, {"b" => 2, "nest" => {'k' => 'v'}}]])],
    "MultiEventStream without copy" => ['no_copy', gen_multi_es.call],
    "MultiEventStream with shallow" => ['shallow', gen_multi_es.call],
    "MultiEventStream with marshal" => ['marshal', gen_multi_es.call],
    "MultiEventStream with deep"    => ['deep',    gen_multi_es.call],
  )
  def test_copy_mode_with_event_streams(data)
    copy_mode, es = data

    d = create_event_test_driver(copy_mode)
    d.run(default_tag: 'test') do
      d.feed(es)
    end

    events = d.instance.outputs.map(&:events)

    if copy_mode != 'no_copy'
      events[0].each_with_index do |entry0, i|
        record0 = entry0.last
        record1 = events[1][i].last

        assert_not_equal record0.object_id, record1.object_id
        assert_equal "bar", record0["foo"]
        assert !record1.has_key?("foo")
        if copy_mode == 'shallow'
          assert_equal record0['nest'].object_id, record1['nest'].object_id
        else
          assert_not_equal record0['nest'].object_id, record1['nest'].object_id
        end
      end
    else
      events[0].each_with_index do |entry0, i|
        record0 = entry0.last
        record1 = events[1][i].last

        assert_equal record0.object_id, record1.object_id
        assert_equal "bar", record0["foo"]
        assert_equal "bar", record1["foo"]
        assert_equal record0['nest'].object_id, record1['nest'].object_id
      end
    end
  end

  IGNORE_ERROR_CONFIG = %[
    <store ignore_error>
      @type test
      name c0
    </store>
    <store ignore_error>
      @type test
      name c1
    </store>
    <store>
      @type test
      name c2
    </store>
  ]

  def test_ignore_error
    d = create_driver(IGNORE_ERROR_CONFIG)

    # override to raise an error
    d.instance.outputs[0].define_singleton_method(:process) do |tag, es|
      raise ArgumentError, 'Failed'
    end

    time = Time.parse("2011-01-02 13:14:15 UTC").to_i
    assert_nothing_raised do
      d.run(default_tag: 'test') do
        d.feed(time, {"a"=>1})
      end
    end
  end

  IGNORE_IF_PREV_SUCCESS_CONFIG = %[
    <store ignore_error>
      @type test
      name c0
    </store>
    <store ignore_if_prev_success ignore_error>
      @type test
      name c1
    </store>
    <store ignore_if_prev_success>
      @type test
      name c2
    </store>
  ]

  def test_ignore_if_prev_success
    d = create_driver(IGNORE_IF_PREV_SUCCESS_CONFIG)

    # override to raise an error
    d.instance.outputs[0].define_singleton_method(:process) do |tag, es|
      raise ArgumentError, 'Failed'
    end

    # check ingore_if_prev_success functionality:
    # 1) output 2 is succeeded.
    # 2) output 3 is not called.
    flexstub(d.instance.outputs[1]) do |output|
      output.should_receive(:process).once
    end
    flexstub(d.instance.outputs[2]) do |output|
      output.should_receive(:process).never
    end

    time = Time.parse("2011-01-02 13:14:15 UTC").to_i
    assert_nothing_raised do
      d.run(default_tag: 'test') do
        d.feed(time, {"a"=>1})
      end
    end
  end

  # --- per-store copy_mode ---

  THREE_STORE_MODES_CONFIG = %[
    copy_mode no_copy
    <store>
      @type copy_recording
      name output_deep
      copy_mode deep
    </store>
    <store>
      @type copy_recording
      name output_shared
    </store>
    <store>
      @type copy_recording
      name output_shallow
      copy_mode shallow
    </store>
  ]

  def create_recording_driver(conf)
    Fluent::Test::Driver::MultiOutput.new(Fluent::Plugin::CopyOutput).configure(conf)
  end

  def test_per_store_copy_mode_replay_comparison
    d = create_recording_driver(THREE_STORE_MODES_CONFIG)
    deep, shared, shallow = d.instance.outputs

    t1 = event_time("2024-01-02 03:04:05 UTC")
    t2 = event_time("2024-01-02 03:04:06 UTC")
    source = Fluent::ArrayEventStream.new([
      [t1, {"seq" => 1, "nest" => {"k" => "v1", "list" => [1, 2]}}],
      [t2, {"seq" => 2, "nest" => {"k" => "v2", "list" => [3, 4]}}],
    ])

    d.run(default_tag: 'test') do
      d.feed(source)
    end

    deep_events = deep.events
    shared_events = shared.events
    shallow_events = shallow.events

    # All stores saw the same events in the same order with the same timestamps.
    assert_equal [t1, t2], deep_events.map(&:first)
    assert_equal [t1, t2], shared_events.map(&:first)
    assert_equal [t1, t2], shallow_events.map(&:first)
    assert_equal [1, 2], deep_events.map { |_, r| r["seq"] }
    assert_equal [1, 2], shared_events.map { |_, r| r["seq"] }
    assert_equal [1, 2], shallow_events.map { |_, r| r["seq"] }

    # The EventTime instances (nanoseconds included) are passed through untouched.
    deep_events.map(&:first).each { |t| assert_equal Fluent::EventTime, t.class }

    # Deep store: record and nested values are independent from the source and the others.
    deep_record = deep_events[0].last
    refute_equal source.first.last.object_id, deep_record.object_id
    refute_equal source.first.last["nest"].object_id, deep_record["nest"].object_id
    refute_equal source.first.last["nest"]["list"].object_id, deep_record["nest"]["list"].object_id

    # Shared (no_copy) store: exactly the source records.
    shared_record = shared_events[0].last
    assert_equal source.first.last.object_id, shared_record.object_id

    # Shallow store: own top-level hash, nested values still shared with the source.
    shallow_record = shallow_events[0].last
    refute_equal source.first.last.object_id, shallow_record.object_id
    assert_equal source.first.last["nest"].object_id, shallow_record["nest"].object_id
  end

  def test_per_store_deep_modification_does_not_leak_to_other_stores
    d = create_recording_driver(THREE_STORE_MODES_CONFIG)
    deep, shared, shallow = d.instance.outputs

    # Only the first store modifies records, including nested values.
    deep.define_singleton_method(:process) do |tag, es|
      es.each do |time, record|
        record["foo"] = "bar"
        record["nest"]["k"] = "modified"
        record["nest"]["list"] << 999
      end
      super(tag, es)
    end

    t = event_time("2024-01-02 03:04:05 UTC")
    source_record = {"seq" => 1, "nest" => {"k" => "v", "list" => [1, 2]}}
    source = Fluent::ArrayEventStream.new([[t, source_record]])

    d.run(default_tag: 'test') do
      d.feed(source)
    end

    # The source and the other stores are untouched, nested values included.
    assert !source_record.has_key?("foo")
    assert_equal "v", source_record["nest"]["k"]
    assert_equal [1, 2], source_record["nest"]["list"]

    shared_record = shared.events[0].last
    assert !shared_record.has_key?("foo")
    assert_equal "v", shared_record["nest"]["k"]
    assert_equal [1, 2], shared_record["nest"]["list"]

    assert !shallow.events[0].last.has_key?("foo")

    # The deep store kept its own modification.
    assert_equal "bar", deep.events[0].last["foo"]
    assert_equal "modified", deep.events[0].last["nest"]["k"]
  end

  def test_store_sees_its_own_modifications_when_iterating_twice
    d = create_recording_driver(%[
      copy_mode no_copy
      <store>
        @type copy_recording
        name output1
        copy_mode deep
      </store>
      <store>
        @type copy_recording
        name output2
      </store>
    ])
    first, second = d.instance.outputs

    seen_on_second_pass = []
    first.define_singleton_method(:process) do |tag, es|
      es.each { |time, record| record["foo"] = "bar" }
      es.each { |time, record| seen_on_second_pass << record["foo"] }
      super(tag, es)
    end

    t = event_time("2024-01-02 03:04:05 UTC")
    source_record = {"seq" => 1}
    d.run(default_tag: 'test') do
      d.feed(t, source_record)
    end

    # The store's own copy persists across its traversals ...
    assert_equal ["bar"], seen_on_second_pass
    # ... but the other store and the source are unaffected.
    assert !second.events[0].last.has_key?("foo")
    assert !source_record.has_key?("foo")
  end

  def test_per_store_no_copy_shares_modifications
    d = create_recording_driver(%[
      copy_mode no_copy
      <store>
        @type copy_recording
        name output1
      </store>
      <store>
        @type copy_recording
        name output2
      </store>
    ])
    first, second = d.instance.outputs
    first.define_singleton_method(:process) do |tag, es|
      es.each { |time, record| record["foo"] = "bar" }
      super(tag, es)
    end

    t = event_time("2024-01-02 03:04:05 UTC")
    d.run(default_tag: 'test') do
      d.feed(t, {"seq" => 1})
    end

    assert_equal "bar", first.events[0].last["foo"]
    assert_equal "bar", second.events[0].last["foo"]
    assert_equal first.events[0].last.object_id, second.events[0].last.object_id
  end

  def test_per_store_marshal_mode_copies_symbols_and_nested_values
    d = create_recording_driver(%[
      <store>
        @type copy_recording
        name output_marshal
        copy_mode marshal
      </store>
      <store>
        @type copy_recording
        name output_shared
      </store>
    ])
    copied, shared = d.instance.outputs

    t = event_time("2024-01-02 03:04:05 UTC")
    source_record = {"sym" => :foo, "nest" => {"list" => [1, {"x" => 2}]}}
    d.run(default_tag: 'test') do
      d.feed(t, source_record)
    end

    copied_record = copied.events[0].last
    assert_equal :foo, copied_record["sym"]
    refute_equal source_record.object_id, copied_record.object_id
    refute_equal source_record["nest"]["list"].object_id, copied_record["nest"]["list"].object_id
    assert_equal source_record.object_id, shared.events[0].last.object_id
  end

  def test_deep_copy_of_streaming_input_is_lazy
    d = create_recording_driver(%[
      copy_mode deep
      <store>
        @type copy_recording
        name output1
        limit 1
      </store>
      <store>
        @type copy_recording
        name output2
        limit 1
      </store>
    ])
    first, second = d.instance.outputs

    # Count how many records the deep copier is asked to copy.
    inner_copier = Fluent::Plugin::CopyOutput.const_get(:DEEP_COPIER)
    copied_count = 0
    counting_copier = ->(record) { copied_count += 1; inner_copier.call(record) }
    d.instance.instance_variable_set(:@copy_procs, [counting_copier, counting_copier])

    t = event_time("2024-01-02 03:04:05 UTC")
    entries = (0...10).map { |i| [t, {"seq" => i}] }
    probe = FluentPluginCopyOutputTest::ProbeEventStream.new(entries)

    # Each store stops after the first event, so only 2 records may be copied,
    # even though the batch contains 10.
    d.run(default_tag: 'test') do
      d.feed(probe)
    end

    assert_equal 1, first.events.size
    assert_equal 1, second.events.size
    assert_equal 2, copied_count
    refute_equal entries[0][1].object_id, first.events[0].last.object_id
  end

  def test_copy_does_not_copy_records_when_nothing_is_consumed
    d = create_recording_driver(%[
      copy_mode deep
      <store>
        @type copy_recording
        name output1
      </store>
      <store>
        @type copy_recording
        name output2
      </store>
    ])
    first, second = d.instance.outputs
    copier = Fluent::Plugin::CopyOutput.const_get(:DEEP_COPIER)
    copies = 0
    counting_copier = ->(record) { copies += 1; copier.call(record) }

    received = []
    [first, second].each do |output|
      output.define_singleton_method(:process) do |tag, es|
        received << es
        # intentionally iterate nothing
      end
    end

    t = event_time("2024-01-02 03:04:05 UTC")
    entries = (0...5).map { |i| [t, {"seq" => i}] }
    probe = FluentPluginCopyOutputTest::ProbeEventStream.new(entries)

    # Drive `process` directly so the configured copy procs are in play.
    d.instance.instance_variable_set(:@copy_procs, [counting_copier, counting_copier])
    d.instance.__send__(:process, 'test', probe)

    assert_equal 2, received.size
    assert_equal 0, copies
    # The streaming input was walked once for fan-out, not once per store.
    assert_equal 1, probe.iterations
  end

  def test_deep_copier_does_not_disturb_thread_local_packer
    copier = Fluent::Plugin::CopyOutput.const_get(:DEEP_COPIER)
    packer = Fluent::MessagePackFactory.thread_local_msgpack_packer

    # Simulate an outer serializer that has already written part of a stream.
    packer.write([1, {"outer" => "first"}])
    copied = copier.call({"nested" => {"k" => "v"}})
    packer.write([2, {"outer" => "second"}])

    packed = packer.full_pack
    unpacked = []
    Fluent::MessagePackFactory.msgpack_unpacker.feed_each(packed) { |entry| unpacked << entry }

    # The outer stream is intact: the deep copy used its own packer.
    assert_equal [[1, {"outer" => "first"}], [2, {"outer" => "second"}]], unpacked
    assert_equal({"nested" => {"k" => "v"}}, copied)
  end

  def test_per_store_mode_with_buffered_memory_store
    d = Fluent::Test::Driver::MultiOutput.new(Fluent::Plugin::CopyOutput).configure(%[
      <store>
        @type copy_recording_memory
        name memory1
        copy_mode deep
        <buffer>
          flush_mode immediate
          chunk_keys tag
        </buffer>
      </store>
      <store>
        @type copy_recording
        name sync1
      </store>
    ])
    memory, sync = d.instance.outputs

    t1 = event_time("2024-01-02 03:04:05.123456789 UTC")
    t2 = event_time("2024-01-02 03:04:06.987654321 UTC")
    source = Fluent::ArrayEventStream.new([
      [t1, {"seq" => 1, "nest" => {"k" => "v"}}],
      [t2, {"seq" => 2, "nest" => {"k" => "v"}}],
    ])

    d.run(default_tag: 'test') do
      d.feed(source)
    end

    # The buffered store writes copies through its (memory) chunk.
    assert_equal [t1, t2], memory.events.map(&:first)
    assert_equal [1, 2], memory.events.map { |_, r| r["seq"] }
    refute_equal source.first.last.object_id, memory.events[0].last.object_id
    assert_equal [t1, t2], sync.events.map(&:first)
  end

  def test_copy_mode_with_single_store_on_streaming_input
    d = create_recording_driver(%[
      copy_mode deep
      <store>
        @type copy_recording
        name output1
      </store>
    ])
    output = d.instance.outputs[0]

    t = event_time("2024-01-02 03:04:05 UTC")
    entries = (0...3).map { |i| [t, {"seq" => i}] }
    probe = FluentPluginCopyOutputTest::ProbeEventStream.new(entries)

    d.run(default_tag: 'test') do
      d.feed(probe)
    end

    assert_equal [0, 1, 2], output.events.map { |_, r| r["seq"] }
    refute_equal entries[0][1].object_id, output.events[0].last.object_id
    # The streaming input is walked once to make it repeatable; copies are lazy.
    assert_equal 1, probe.iterations
  end

  # The scenario from the request: three in-memory (buffered) stores replaying the
  # same batch of events, only some of which modify records. Compare what each
  # store ended up receiving.
  def test_three_memory_stores_replay_same_events
    d = Fluent::Test::Driver::MultiOutput.new(Fluent::Plugin::CopyOutput).configure(%[
      copy_mode no_copy
      <store>
        @type copy_recording_memory
        name deep_modifier
        copy_mode deep
        modify true
        <buffer>
          flush_mode immediate
          chunk_keys tag
        </buffer>
      </store>
      <store>
        @type copy_recording_memory
        name shared_reader
        <buffer>
          flush_mode immediate
          chunk_keys tag
        </buffer>
      </store>
      <store>
        @type copy_recording_memory
        name shallow_modifier
        copy_mode shallow
        <buffer>
          flush_mode immediate
          chunk_keys tag
        </buffer>
      </store>
    ])
    deep, shared, shallow = d.instance.outputs

    t1 = event_time("2024-03-04 05:06:07.000000001 UTC")
    t2 = event_time("2024-03-04 05:06:08.000000002 UTC")
    source_records = [
      {"seq" => 1, "nest" => {"k" => "original"}},
      {"seq" => 2, "nest" => {"k" => "original"}},
    ]
    source = Fluent::ArrayEventStream.new([[t1, source_records[0]], [t2, source_records[1]]])

    d.run(default_tag: 'test') do
      d.feed(source)
    end

    # Every store got the same batch, in the same order, with the same EventTimes
    # (nanoseconds included).
    [deep, shared, shallow].each do |store|
      assert_equal [t1, t2], store.events.map(&:first)
      assert_equal [1, 2], store.events.map { |_, r| r["seq"] }
      store.events.map(&:first).each do |time|
        assert_equal Fluent::EventTime, time.class
      end
      assert_equal [1, 2], store.events.map { |t, _| t.nsec }
    end

    # The deep store modified its own records only, nested values included.
    assert_equal ["deep_modifier", "deep_modifier"], deep.events.map { |_, r| r["seen_by"] }
    assert_equal ["deep_modifier", "deep_modifier"], deep.events.map { |_, r| r["nest"]["k"] }

    # The shared reader and the source saw none of those modifications.
    assert_equal [nil, nil], shared.events.map { |_, r| r["seen_by"] }
    assert_equal ["original", "original"], shared.events.map { |_, r| r["nest"]["k"] }
    assert_equal ["original", "original"], source_records.map { |r| r["nest"]["k"] }
    assert source_records.none? { |r| r.has_key?("seen_by") }

    # The shallow store also saw the untouched records, nested values included.
    shallow.events.each_with_index do |(_, record), i|
      assert !record.has_key?("seen_by")
      assert_equal ["seq"], record.keys - ["nest"]
      assert_equal source_records[i]["nest"]["k"], record["nest"]["k"]
    end
  end
end

