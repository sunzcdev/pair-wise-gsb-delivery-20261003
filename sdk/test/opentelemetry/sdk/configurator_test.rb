# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'
require 'opentelemetry/exporter/zipkin'

describe OpenTelemetry::SDK::Configurator do
  let(:configurator) { OpenTelemetry::SDK::Configurator.new }
  let(:default_resource_attributes) do
    {
      'telemetry.sdk.name' => 'opentelemetry',
      'telemetry.sdk.language' => 'ruby',
      'telemetry.sdk.version' => OpenTelemetry::SDK::VERSION,
      'process.pid' => Process.pid,
      'process.command' => $PROGRAM_NAME,
      'process.runtime.name' => RUBY_ENGINE,
      'process.runtime.version' => RUBY_VERSION,
      'process.runtime.description' => RUBY_DESCRIPTION,
      'service.name' => 'unknown_service'
    }
  end

  describe '#logger' do
    # Reset the logger
    after { OpenTelemetry.logger = Logger.new(File::NULL) }

    it 'returns a logger instance' do
      _(configurator.logger).must_be_instance_of(Logger)
    end

    it 'assigns the logger to OpenTelemetry.logger, with a default level of info' do
      custom_logger = Logger.new(File::NULL, level: 'INFO')
      _(OpenTelemetry.logger).wont_equal custom_logger

      OpenTelemetry::SDK.configure { |c| c.logger = custom_logger }
      _(OpenTelemetry.logger.instance_variable_get(:@logger)).must_equal custom_logger
      _(OpenTelemetry.logger.instance_variable_get(:@level)).must_equal Logger::INFO
      _(OpenTelemetry.logger).must_be_instance_of(OpenTelemetry::SDK::ForwardingLogger)
    end

    it 'respects the supplied loggers severity level' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        custom_logger = Logger.new(log_stream, level: 'ERROR')
        OpenTelemetry::SDK.configure { |c| c.logger = custom_logger }

        OpenTelemetry.logger.debug('The forwarding logger should forward this message')
        _(log_stream.string).must_be_empty
      end
    end

    it 'allows control of the otel log level' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        custom_logger = Logger.new(log_stream, level: 'DEBUG')

        OpenTelemetry::TestHelpers.with_env('OTEL_LOG_LEVEL' => 'ERROR') do
          OpenTelemetry::SDK.configure { |c| c.logger = custom_logger }
        end

        OpenTelemetry.logger.warn('The forwarding logger should not forward this message')
        _(log_stream.string).must_be_empty
      end
    end

    it 'forwards log messages supplied as parameters' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        custom_logger = Logger.new(log_stream)
        OpenTelemetry::SDK.configure { |c| c.logger = custom_logger }
        message = 'The forwarding logger should forward this message'
        OpenTelemetry.logger.info(message)
        _(log_stream.string).must_include message
      end
    end

    it 'forwards log messages supplied in blocks' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        custom_logger = Logger.new(log_stream)
        OpenTelemetry::SDK.configure { |c| c.logger = custom_logger }
        message = 'The forwarding logger should forward this message'
        OpenTelemetry.logger.info { message }
        _(log_stream.string).must_include message
      end
    end
  end

  describe '#resource=' do
    let(:configurator_resource) { configurator.instance_variable_get(:@resource) }
    let(:configurator_resource_attributes) { configurator_resource.attribute_enumerator.to_h }
    let(:expected_resource_attributes) { default_resource_attributes.merge('test_key' => 'test_value') }

    it 'merges the resource' do
      configurator.resource = OpenTelemetry::SDK::Resources::Resource.create('test_key' => 'test_value')
      _(configurator_resource_attributes).must_equal(expected_resource_attributes)
    end

    describe 'when there is a resource key collision' do
      let(:expected_resource_attributes) { default_resource_attributes.merge('important_value' => '25') }

      after do
        OpenTelemetry::SDK::Resources::Resource.instance_variable_set(:@default, nil)
      end

      it 'uses the user provided resources' do
        OpenTelemetry::TestHelpers.with_env('OTEL_RESOURCE_ATTRIBUTES' => 'important_value=100') do
          configurator.resource = OpenTelemetry::SDK::Resources::Resource.create('important_value' => '25')
          _(configurator_resource_attributes).must_equal(expected_resource_attributes)
        end
      end
    end
  end

  describe '#service_name=' do
    let(:configurator_resource) { configurator.instance_variable_get(:@resource) }
    let(:configurator_resource_attributes) { configurator_resource.attribute_enumerator.to_h }
    let(:expected_resource_attributes) { default_resource_attributes.merge('service.name' => 'Otel Demo App') }

    it 'assigns the service_name resource' do
      configurator.service_name = 'Otel Demo App'
      _(configurator_resource_attributes).must_equal(expected_resource_attributes)
    end
  end

  describe '#service_version=' do
    let(:configurator_resource) { configurator.instance_variable_get(:@resource) }
    let(:configurator_resource_attributes) { configurator_resource.attribute_enumerator.to_h }
    let(:expected_resource_attributes) { default_resource_attributes.merge('service.version' => '0.6.0') }

    it 'assigns the service_version resource' do
      configurator.service_version = '0.6.0'
      _(configurator_resource_attributes).must_equal(expected_resource_attributes)
    end
  end

  describe '#add_resource_detector' do
    it 'rejects detectors that do not respond to #detect' do
      _(-> { configurator.add_resource_detector(Object.new) }).must_raise(ArgumentError)
    end

    it 'returns the detector for chaining' do
      detector = ResourceDetectorDoubles::Fast.new('k' => 'v')
      _(configurator.add_resource_detector(detector)).must_be_same_as(detector)
    end
  end

  describe '#resource_detector_concurrency=' do
    it 'rejects non-positive and non-integer limits' do
      _(-> { configurator.resource_detector_concurrency = 0 }).must_raise(ArgumentError)
      _(-> { configurator.resource_detector_concurrency = -1 }).must_raise(ArgumentError)
      _(-> { configurator.resource_detector_concurrency = 1.5 }).must_raise(ArgumentError)
    end
  end

  describe '#resource_detection_timeout=' do
    it 'rejects non-positive and non-numeric deadlines' do
      _(-> { configurator.resource_detection_timeout = 0 }).must_raise(ArgumentError)
      _(-> { configurator.resource_detection_timeout = -0.1 }).must_raise(ArgumentError)
      _(-> { configurator.resource_detection_timeout = '5' }).must_raise(ArgumentError)
    end

    it 'accepts an explicit nil deadline' do
      configurator.resource_detection_timeout = nil
      _(configurator.instance_variable_get(:@resource_detection_timeout)).must_be_nil
    end
  end

  describe 'resource detection' do
    let(:gate) { Queue.new }

    after { gate.close unless gate.closed? }

    let(:detected_attributes) { OpenTelemetry.tracer_provider.resource.attribute_enumerator.to_h }

    it 'merges detected attributes in declaration order regardless of completion order' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('shared' => 'first', 'first.only' => 'yes'))
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('shared' => 'second', 'second.only' => 'yes'))

      configurator.configure

      _(detected_attributes['shared']).must_equal('second')
      _(detected_attributes['first.only']).must_equal('yes')
      _(detected_attributes['second.only']).must_equal('yes')
    end

    it 'keeps settled results when a detector is still stuck at the deadline' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('fast.key' => 'fast-value'))
      configurator.add_resource_detector(ResourceDetectorDoubles::Stuck.new({ 'stuck.key' => 'stuck-value' }, gate))
      configurator.add_resource_detector(ResourceDetectorDoubles::Failing.new(RuntimeError.new('detector exploded')))
      configurator.resource_detection_timeout = 0.2

      started = OpenTelemetry::Common::Utilities.timeout_timestamp
      configurator.configure
      elapsed = OpenTelemetry::Common::Utilities.timeout_timestamp - started

      _(elapsed).must_be :<, 5
      _(detected_attributes['fast.key']).must_equal('fast-value')
      _(detected_attributes).wont_include('stuck.key')
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[success timeout error])
    end

    it 'creates a usable tracer from the settled results of fast, stuck and failing detectors' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('shared.name' => 'from-fast', 'fast.only' => 'yes'))
      configurator.add_resource_detector(ResourceDetectorDoubles::Stuck.new({ 'shared.name' => 'from-stuck', 'stuck.only' => 'yes' }, gate))
      configurator.add_resource_detector(ResourceDetectorDoubles::Failing.new(RuntimeError.new('detector exploded')))
      configurator.resource_detection_timeout = 0.2

      configurator.configure

      tracer = OpenTelemetry.tracer_provider.tracer('resource-detection', '1.0')
      span = tracer.start_span('probe')
      attributes = span.resource.attribute_enumerator.to_h

      _(span.context).wont_be_nil
      _(attributes['shared.name']).must_equal('from-fast')
      _(attributes['fast.only']).must_equal('yes')
      _(attributes).wont_include('stuck.only')
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[success timeout error])
      span.finish
    end

    it 'applies declaration order rather than completion order for colliding keys' do
      # The later-declared detector returns almost immediately; the
      # earlier-declared one returns afterwards. Completion order is therefore
      # reversed, but the later declaration must still win the collision.
      configurator.add_resource_detector(ResourceDetectorDoubles::Slow.new({ 'shared.name' => 'early-declared' }, 0.05))
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('shared.name' => 'late-declared'))

      configurator.configure

      _(detected_attributes['shared.name']).must_equal('late-declared')
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[success success])
    end

    it 'discards attributes from a detector that returns after the deadline' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('fast.key' => 'fast-value'))
      configurator.add_resource_detector(ResourceDetectorDoubles::Stuck.new({ 'stuck.key' => 'stuck-value' }, gate))
      configurator.resource_detection_timeout = 0.2

      configurator.configure
      resource = OpenTelemetry.tracer_provider.resource
      before = resource.attribute_enumerator.to_h

      gate << true # release the stuck detector after configuration completed
      sleep 0.1     # let it come back and attempt to write into the run

      _(resource.attribute_enumerator.to_h).must_equal(before)
      _(resource.attribute_enumerator.to_h).wont_include('stuck.key')
      _(OpenTelemetry.tracer_provider.resource).must_be_same_as(resource)
    end

    it 'records detector failures without aborting configuration' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Failing.new(ArgumentError.new('boom')))
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('fast.key' => 'fast-value'))

      configurator.configure

      results = configurator.resource_detection_results
      _(results.map(&:status)).must_equal(%i[error success])
      _(results.first.error).must_be_kind_of(ArgumentError)
      _(detected_attributes['fast.key']).must_equal('fast-value')
    end

    it 'lets an explicitly configured resource override detected attributes' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('shared' => 'detected'))
      configurator.resource = OpenTelemetry::SDK::Resources::Resource.create('shared' => 'configured')

      configurator.configure

      _(detected_attributes['shared']).must_equal('configured')
    end

    it 'accepts hashes and nil as detector results' do
      configurator.add_resource_detector(ResourceDetectorDoubles::HashResult.new('hash.key' => 'hash-value'))
      configurator.add_resource_detector(ResourceDetectorDoubles::NilResult.new)

      configurator.configure

      _(detected_attributes['hash.key']).must_equal('hash-value')
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[success success])
    end

    it 'reports an unsupported detector result as an error' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Invalid.new)
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('fast.key' => 'fast-value'))

      configurator.configure

      results = configurator.resource_detection_results
      _(results.map(&:status)).must_equal(%i[error success])
      _(results.first.error).must_be_kind_of(TypeError)
      _(detected_attributes['fast.key']).must_equal('fast-value')
    end

    it 'waits for every detector when no deadline is set' do
      configurator.resource_detection_timeout = nil
      configurator.add_resource_detector(ResourceDetectorDoubles::Slow.new({ 'slow.key' => 'slow-value' }, 0.05))
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('fast.key' => 'fast-value'))

      configurator.configure

      _(detected_attributes['slow.key']).must_equal('slow-value')
      _(detected_attributes['fast.key']).must_equal('fast-value')
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[success success])
    end

    it 'does not leave the resource with an in-flight detector result' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Stuck.new({ 'stuck.key' => 'stuck-value' }, gate))
      configurator.resource_detection_timeout = 0.1

      configurator.configure

      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[timeout])
      _(detected_attributes).wont_include('stuck.key')
    end

    it 'does not wait forever when a detector aborts its worker entirely' do
      configurator.add_resource_detector(ResourceDetectorDoubles::Aborting.new)
      configurator.add_resource_detector(ResourceDetectorDoubles::Fast.new('fast.key' => 'fast-value'))
      configurator.resource_detector_concurrency = 1
      configurator.resource_detection_timeout = nil

      runner = Thread.new { configurator.configure }
      _(runner.join(5)).wont_be_nil

      # The worker died before running the second detector, so that detector
      # never produced a result; it must be reported, not awaited forever.
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[timeout timeout])
    end

    it 'runs at most the configured number of detectors concurrently' do
      tracker = ResourceDetectorDoubles::ConcurrencyTracker.new
      detector = ResourceDetectorDoubles::Tracked.new(tracker)

      4.times { configurator.add_resource_detector(detector) }
      configurator.resource_detector_concurrency = 2

      configurator.configure

      _(tracker.peak).must_be :<=, 2
      _(configurator.resource_detection_results.map(&:status)).must_equal(%i[success success success success])
    end
  end

  describe '#use' do
    it 'can be called multiple times' do
      configurator.use('TestInstrumentation', enabled: true)
      configurator.use('TestInstrumentation1')
    end
  end

  describe '#use, #use_all' do
    describe 'should be used mutually exclusively' do
      it 'raises when use_all called after use' do
        configurator.use('TestInstrumentation', enabled: true)
        _(-> { configurator.use_all }).must_raise(StandardError)
      end

      it 'raises when use called after use_all' do
        configurator.use_all
        _(-> { configurator.use('TestInstrumentation', enabled: true) })
          .must_raise(StandardError)
      end
    end
  end

  describe '#configure' do
    describe 'propagators' do
      it 'defaults to trace context and baggage' do
        configurator.configure

        expected_propagators = [
          OpenTelemetry::Trace::Propagation::TraceContext.text_map_propagator,
          OpenTelemetry::Baggage::Propagation.text_map_propagator
        ]

        _(propagators_for(OpenTelemetry.propagation)).must_equal(expected_propagators)
      end

      it 'is user settable' do
        propagator = OpenTelemetry::SDK::Configurator::NoopTextMapPropagator.new
        configurator.propagators = [propagator]
        configurator.configure

        _(OpenTelemetry.propagation).must_equal(propagator)
      end

      it 'can be set by environment variable' do
        OpenTelemetry::TestHelpers.with_env('OTEL_PROPAGATORS' => 'baggage') do
          configurator.configure
        end

        _(OpenTelemetry.propagation).must_equal(OpenTelemetry::Baggage::Propagation.text_map_propagator)
      end

      it 'supports "none" as an environment variable' do
        OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
          OpenTelemetry::TestHelpers.with_env('OTEL_PROPAGATORS' => 'none') do
            configurator.configure
          end

          _(OpenTelemetry.propagation).must_be_instance_of(
            OpenTelemetry::SDK::Configurator::NoopTextMapPropagator
          )

          _(log_stream.string).wont_match(/The none propagator is unknown and cannot be configured/)
        end
      end

      it 'defaults to noop with invalid env var' do
        OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
          OpenTelemetry::TestHelpers.with_env('OTEL_PROPAGATORS' => 'unladen_swallow') do
            configurator.configure
          end

          _(OpenTelemetry.propagation).must_be_instance_of(
            OpenTelemetry::SDK::Configurator::NoopTextMapPropagator
          )

          _(log_stream.string).must_match(/The unladen_swallow propagator is unknown and cannot be configured/)
        end
      end
    end

    describe 'tracer_provider' do
      it 'is an instance of SDK::Trace::TracerProvider' do
        configurator.configure

        _(OpenTelemetry.tracer_provider).must_be_instance_of(
          OpenTelemetry::SDK::Trace::TracerProvider
        )
      end

      it 'reflects the configured id generator' do
        id_generator = Object.new
        configurator.id_generator = id_generator
        configurator.configure

        _(OpenTelemetry.tracer_provider.id_generator).must_equal id_generator
      end
    end

    describe 'span processors' do
      it 'defaults to no processors if no valid exporter is available' do
        configurator.configure

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)).must_be_empty
      end

      it 'reflects configured value' do
        configurator.add_span_processor(
          OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor.new(
            OpenTelemetry::SDK::Trace::Export::ConsoleSpanExporter.new
          )
        )

        configurator.configure

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors).first).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor
        )
      end

      it 'can be set by environment variable' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'zipkin') do
          configurator.configure
        end

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors).first).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors).first.instance_variable_get(:@exporter)).must_be_instance_of(
          OpenTelemetry::Exporter::Zipkin::Exporter
        )
      end

      it 'accepts "none" as an environment variable value' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'none') do
          configurator.configure
        end

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)).must_be_empty
      end

      it 'accepts comma separated list as an environment variable' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'zipkin,console') do
          configurator.configure
        end

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[0]).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[0].instance_variable_get(:@exporter)).must_be_instance_of(
          OpenTelemetry::Exporter::Zipkin::Exporter
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[1]).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[1].instance_variable_get(:@span_exporter)).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::ConsoleSpanExporter
        )
      end

      it 'accepts comma separated list with preceding or trailing spaces as an environment variable' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'zipkin , console') do
          configurator.configure
        end

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[0]).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::BatchSpanProcessor
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[0].instance_variable_get(:@exporter)).must_be_instance_of(
          OpenTelemetry::Exporter::Zipkin::Exporter
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[1]).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors)[1].instance_variable_get(:@span_exporter)).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::ConsoleSpanExporter
        )
      end

      it 'accepts "console" as an environment variable value' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'console') do
          configurator.configure
        end

        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors).first).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::SimpleSpanProcessor
        )
        _(OpenTelemetry.tracer_provider.instance_variable_get(:@span_processors).first.instance_variable_get(:@span_exporter)).must_be_instance_of(
          OpenTelemetry::SDK::Trace::Export::ConsoleSpanExporter
        )
      end

      it 'warns on unsupported otlp transport protocol grpc' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'otlp', 'OTEL_EXPORTER_OTLP_TRACES_PROTOCOL' => 'grpc') do
          OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
            configurator.configure

            _(log_stream.string).must_match(/The grpc transport protocol is not supported by the OTLP exporter/)
          end
        end
      end

      it 'warns on unsupported otlp transport protocol http/json' do
        OpenTelemetry::TestHelpers.with_env('OTEL_TRACES_EXPORTER' => 'otlp', 'OTEL_EXPORTER_OTLP_TRACES_PROTOCOL' => 'http/json') do
          OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
            configurator.configure

            _(log_stream.string).must_match(%r{The http/json transport protocol is not supported by the OTLP exporter})
          end
        end
      end
    end

    describe 'instrumentation installation' do
      before do
        OpenTelemetry::Instrumentation.instance_variable_set(:@registry, nil)
        TestInstrumentation = Class.new(OpenTelemetry::Instrumentation::Base) do
          install { 1 + 1 }
          present { true }
          option :opt, default: false, validate: :boolean
        end
      end

      after do
        Object.send(:remove_const, :TestInstrumentation)
      end

      it 'installs single instrumentation' do
        registry = OpenTelemetry::Instrumentation.registry
        instrumentation = registry.lookup('TestInstrumentation')
        _(instrumentation).wont_be_nil
        _(instrumentation).wont_be(:installed?)
        configurator.use 'TestInstrumentation', opt: true
        configurator.configure
        _(instrumentation).must_be(:installed?)
        _(instrumentation.config).must_equal({ opt: true })
      end

      it 'installs all' do
        registry = OpenTelemetry::Instrumentation.registry
        instrumentation = registry.lookup('TestInstrumentation')
        _(instrumentation).wont_be_nil
        _(instrumentation).wont_be(:installed?)
        configurator.use_all 'TestInstrumentation' => { opt: true }
        configurator.configure
        _(instrumentation).must_be(:installed?)
        _(instrumentation.config).must_equal({ opt: true })
      end
    end
  end

  def propagators_for(propagator)
    if propagator.instance_of? Context::Propagation::CompositeTextMapPropagator
      propagator.instance_variable_get(:@propagators)
    else
      [propagator]
    end
  end
end

# Stand-in resource detectors used by the resource detection tests. They live
# outside the +describe+ block so every example shares one definition.
module ResourceDetectorDoubles
  # Returns a fixed resource as soon as it runs.
  class Fast
    def initialize(attributes)
      @attributes = attributes
    end

    def detect
      OpenTelemetry::SDK::Resources::Resource.create(@attributes)
    end
  end

  # Waits briefly, then returns a fixed resource, so it finishes after a
  # concurrently-running {Fast} detector declared later.
  class Slow
    def initialize(attributes, delay)
      @attributes = attributes
      @delay = delay
    end

    def detect
      sleep @delay
      OpenTelemetry::SDK::Resources::Resource.create(@attributes)
    end
  end

  # Blocks until the injected queue is pushed, then returns its resource.
  class Stuck
    def initialize(attributes, gate)
      @attributes = attributes
      @gate = gate
    end

    def detect
      @gate.pop
      OpenTelemetry::SDK::Resources::Resource.create(@attributes)
    end
  end

  # Raises the injected exception as soon as it runs.
  class Failing
    def initialize(error)
      @error = error
    end

    def detect
      raise @error
    end
  end

  # Returns a plain Hash of attributes.
  class HashResult
    def initialize(attributes)
      @attributes = attributes
    end

    def detect
      @attributes
    end
  end

  # Returns nil, which means no attributes.
  class NilResult
    def detect; end
  end

  # Returns something that is neither a Resource nor a Hash.
  class Invalid
    def detect
      :not_a_resource
    end
  end

  # Terminates its own worker thread, so the worker exits without recording a
  # result for this detector or any still queued behind it.
  class Aborting
    def detect
      Thread.current.kill
    end
  end

  # Tracks the highest number of overlapping detector calls.
  class ConcurrencyTracker
    attr_reader :peak

    def initialize
      @mutex = Mutex.new
      @live = 0
      @peak = 0
    end

    def enter
      @mutex.synchronize do
        @live += 1
        @peak = @live if @live > @peak
      end
    end

    def leave
      @mutex.synchronize { @live -= 1 }
    end
  end

  # Reports overlap to a {ConcurrencyTracker} and returns an empty resource.
  class Tracked
    def initialize(tracker)
      @tracker = tracker
    end

    def detect
      @tracker.enter
      sleep 0.05
      OpenTelemetry::SDK::Resources::Resource.create
    ensure
      @tracker.leave
    end
  end
end
