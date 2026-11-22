# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    # The configurator provides defaults and facilitates configuring the
    # SDK for use.
    class Configurator # rubocop:disable Metrics/ClassLength
      # @api private
      class NoopTextMapPropagator
        EMPTY_LIST = [].freeze
        private_constant(:EMPTY_LIST)

        # No-op because this propagator does not inject any values.
        def inject(carrier, context: Context.current, setter: Context::Propagation.text_map_setter); end

        # Returns the passed context unchanged because there is nothing to extract.
        def extract(carrier, context: Context.current, getter: Context::Propagation.text_map_getter)
          context
        end

        # Returns an empty list because this propagator does not use carrier fields.
        def fields
          EMPTY_LIST
        end
      end

      USE_MODE_UNSPECIFIED = 0
      USE_MODE_ONE = 1
      USE_MODE_ALL = 2

      DEFAULT_RESOURCE_DETECTOR_CONCURRENCY = 4
      DEFAULT_RESOURCE_DETECTION_TIMEOUT = 5.0

      private_constant :USE_MODE_UNSPECIFIED, :USE_MODE_ONE, :USE_MODE_ALL,
                       :DEFAULT_RESOURCE_DETECTOR_CONCURRENCY, :DEFAULT_RESOURCE_DETECTION_TIMEOUT

      # The outcome of running a single resource detector during {#configure}.
      #
      # The status is +:success+, +:error+ or +:timeout+. Successful results
      # carry the detected {Resources::Resource}; failed results carry the
      # raised exception in +error+; timed out detectors have neither, because
      # they did not return before the detection deadline.
      DetectorResult = Struct.new(:detector, :status, :resource, :error, keyword_init: true) do
        def success?
          status == :success
        end

        def timeout?
          status == :timeout
        end
      end

      attr_writer :propagators, :id_generator

      # @return [Array<DetectorResult>, nil] the outcome of each resource
      #   detector, in declaration order, or nil before {#configure} ran.
      attr_reader :resource_detection_results

      def initialize
        @instrumentation_names = []
        @instrumentation_config_map = {}
        @propagators = nil
        @span_processors = []
        @use_mode = USE_MODE_UNSPECIFIED
        @default_resource = Resources::Resource.default
        @user_resource = Resources::Resource.create
        @detected_resource = Resources::Resource.create
        @resource = @default_resource
        @resource_detectors = []
        @resource_detector_concurrency = DEFAULT_RESOURCE_DETECTOR_CONCURRENCY
        @resource_detection_timeout = DEFAULT_RESOURCE_DETECTION_TIMEOUT
        @resource_detection_results = nil
        @id_generator = OpenTelemetry::Trace
      end

      # Returns the configured logger or the global OpenTelemetry logger.
      def logger
        @logger ||= OpenTelemetry.logger
      end

      # Accepts a logger and wraps it in the {ForwardingLogger} which allows
      # for controlling the severity level emitted by the OpenTelemetry.logger
      # independently of the supplied logger.
      #
      # @param [Logger] new_logger The logger for OpenTelemetry to use
      def logger=(new_logger)
        @logger = ForwardingLogger.new(new_logger, level: ENV['OTEL_LOG_LEVEL'] || Logger::INFO)
      end

      # Configures the error handler that will be installed on
      # OpenTelemetry during SDK configuration.
      #
      # Assigned object must respond to +#call+ and accept the keyword
      # arguments +exception:+ and +message:+.
      #
      # @param [#call] error_handler The error handler to install
      #
      # @example Configure a custom error handler during SDK setup
      #   OpenTelemetry::SDK.configure do |c|
      #     c.error_handler = lambda do |exception: nil, message: nil|
      #       OpenTelemetry.logger.warn("otel: #{[message, exception&.message].compact.join(' - ')}")
      #     end
      #   end
      # rubocop:disable-next Style/TrivialAccessors
      def error_handler=(error_handler)
        @error_handler = error_handler
      end

      # Returns the configured error handler or the global OpenTelemetry error handler.
      def error_handler
        @error_handler ||= OpenTelemetry.error_handler
      end

      # Accepts a resource object that is merged with the default telemetry sdk
      # resource. The use of this method is optional, and is provided as means
      # to include additional resource information.
      # If a resource key collision occurs the passed in resource takes priority.
      #
      # @param [Resource] new_resource The resource to be merged
      def resource=(new_resource)
        @user_resource = @user_resource.merge(new_resource)
        @resource = assemble_resource
      end

      # Accepts a string that is merged in as the service.name resource attribute.
      # The most recent assigned value will be used in the event of repeated
      # calls to this setter.
      # @param [String] service_name The value to be used as the service name
      def service_name=(service_name)
        self.resource = OpenTelemetry::SDK::Resources::Resource.create(
          OpenTelemetry::SemanticConventions::Resource::SERVICE_NAME => service_name
        )
      end

      # Accepts a string that is merged in as the service.version resource attribute.
      # The most recent assigned value will be used in the event of repeated
      # calls to this setter.
      # @param [String] service_version The value to be used as the service version
      def service_version=(service_version)
        self.resource = OpenTelemetry::SDK::Resources::Resource.create(
          OpenTelemetry::SemanticConventions::Resource::SERVICE_VERSION => service_version
        )
      end

      # Registers a resource detector to run during {#configure}.
      #
      # Detectors run concurrently, bounded by {#resource_detector_concurrency=},
      # and are given until {#resource_detection_timeout=} elapses in aggregate.
      # Their results are merged in declaration order after detection finishes,
      # so which detector finishes first does not affect attribute precedence.
      # A detector that has not returned by the deadline is recorded with a
      # +:timeout+ status; if it returns later, its attributes are discarded and
      # cannot change the resource already in use.
      #
      # @param [#detect] detector An object or class responding to +detect+ and
      #   returning a {Resources::Resource}, a Hash of attributes, or nil.
      # @return [#detect] the detector, for chaining
      def add_resource_detector(detector)
        raise ArgumentError, 'resource detector must respond to #detect' unless detector.respond_to?(:detect)

        @resource_detectors << detector
        detector
      end

      # Sets the maximum number of resource detectors that may run at once.
      #
      # @param [Integer] limit The maximum concurrency; must be positive.
      def resource_detector_concurrency=(limit)
        raise ArgumentError, 'resource detector concurrency must be a positive integer' unless limit.is_a?(Integer) && limit.positive?

        @resource_detector_concurrency = limit
      end

      # Sets the overall deadline, in seconds, for the resource detection phase.
      #
      # The deadline covers all detectors together, not each one individually.
      # Detectors still running when it expires are skipped and recorded with a
      # +:timeout+ status. Because Ruby cannot interrupt a running method, a
      # timed out detector's thread keeps running until it returns, but its
      # result is discarded and never touches the configured resource.
      #
      # @param [Numeric, nil] seconds The deadline in seconds, or nil to wait for
      #   every detector to return.
      def resource_detection_timeout=(seconds)
        valid = seconds.nil? || (seconds.is_a?(Numeric) && seconds.positive?)
        raise ArgumentError, 'resource detection timeout must be a positive number or nil' unless valid

        @resource_detection_timeout = seconds
      end

      # Install an instrumentation with specified optional +config+.
      # Use can be called multiple times to install multiple instrumentation.
      # Only +use+ or +use_all+, but not both when installing
      # instrumentation. A call to +use_all+ after +use+ will result in an
      # exception.
      #
      # @param [String] instrumentation_name The name of the instrumentation
      # @param [optional Hash] config The config for this instrumentation
      def use(instrumentation_name, config = nil)
        check_use_mode!(USE_MODE_ONE)
        @instrumentation_names << instrumentation_name
        @instrumentation_config_map[instrumentation_name] = config if config
      end

      # Install all registered instrumentation. Configuration for specific
      # instrumentation can be provided with the optional +instrumentation_config_map+
      # parameter. Only +use+ or +use_all+, but not both when installing
      # instrumentation. A call to +use+ after +use_all+ will result in an
      # exception.
      #
      # @param [optional Hash<String,Hash>] instrumentation_config_map A map with string keys
      #   representing the instrumentation name and values specifying the instrumentation config
      def use_all(instrumentation_config_map = {})
        check_use_mode!(USE_MODE_ALL)
        @instrumentation_config_map = instrumentation_config_map
      end

      # Add a span processor to the export pipeline
      #
      # @param [#on_start, #on_finish, #shutdown, #force_flush] span_processor A span_processor
      #   that satisfies the duck type #on_start, #on_finish, #shutdown, #force_flush. See
      #   {SimpleSpanProcessor} for an example.
      def add_span_processor(span_processor)
        @span_processors << span_processor
      end

      # Add a log record processor to the export pipeline
      #
      # @param [#emit, #shutdown, #force_flush] log_record_processor A log_record_processor
      #   that satisfies the duck type #emit, #shutdown, #force_flush. See
      #   {SimpleLogRecordProcessor} for an example.
      def add_log_record_processor(log_record_processor); end

      # @api private
      # The configure method is where we define the setup process. This allows
      # us to make certain guarantees about which systems and globals are setup
      # at each stage. The setup process is:
      #   - setup logging
      #   - setup propagation
      #   - detect resource attributes
      #   - setup tracer_provider, meter_provider, and logger_provider
      #   - install instrumentation
      def configure
        OpenTelemetry.logger = logger
        OpenTelemetry.error_handler = error_handler
        configure_propagation
        detect_resource_attributes
        configure_span_processors
        tracer_provider.id_generator = @id_generator
        OpenTelemetry.tracer_provider = tracer_provider
        metrics_configuration_hook
        logs_configuration_hook
        install_instrumentation
      end

      private

      # @api private
      # Coordinates a single bounded, deadline-driven resource detection run.
      # Results are written by worker threads and read once the run is closed.
      # Closing the run is what makes a late result inert: any result recorded
      # after that point is dropped by {#record}, so a detector that outlives
      # the deadline can never alter a resource that is already in use.
      class DetectionState
        def initialize(size, workers)
          @mutex = Mutex.new
          @condition = ConditionVariable.new
          @results = Array.new(size)
          @pending = size
          @workers = workers
          @accepting = true
        end

        # Stores a detector result if the run still accepts them.
        #
        # @return [DetectorResult, nil] the stored result, or nil when it
        #   arrived after the deadline and must be discarded so it cannot alter
        #   a resource that is already in use.
        def record(index, result)
          @mutex.synchronize do
            return nil unless @accepting

            @results[index] = result
            @pending -= 1
            @condition.broadcast
            result
          end
        end

        # @return [Boolean] false once {#finish} has closed the run, so workers
        #   stop picking up detectors whose results could no longer be used.
        def accepting?
          @mutex.synchronize { @accepting }
        end

        # Signals that a worker will not record anything further, so a run with
        # no workers left cannot wait forever on results that can never arrive.
        def worker_finished
          @mutex.synchronize do
            @workers -= 1
            @condition.broadcast
          end
        end

        # Waits for outstanding detectors until +deadline+ (a monotonic
        # timestamp) or indefinitely when +deadline+ is nil, closes the run, and
        # returns the results in declaration order. Detectors that did not
        # return in time are marked +:timeout+.
        #
        # @return [Array<DetectorResult>]
        def finish(detectors, deadline)
          @mutex.synchronize do
            while @pending.positive? && @workers.positive?
              timeout = deadline && (deadline - OpenTelemetry::Common::Utilities.timeout_timestamp)
              break if timeout && !timeout.positive?

              @condition.wait(@mutex, timeout)
            end

            @accepting = false
            @results.each_index do |index|
              @results[index] ||= DetectorResult.new(detector: detectors[index], status: :timeout)
            end
            @results.freeze
          end
        end
      end

      private_constant :DetectionState

      def metrics_configuration_hook; end

      def logs_configuration_hook; end

      def tracer_provider
        @tracer_provider ||= Trace::TracerProvider.new(resource: @resource)
      end

      # Runs every registered resource detector, then folds the results into a
      # single resource in declaration order. Later declarations take
      # precedence, independent of which detector returned first.
      def detect_resource_attributes
        return if @resource_detection_results

        detectors = @resource_detectors.dup
        @resource_detection_results = run_resource_detectors(detectors)
        @resource_detection_results.each do |result|
          next unless result.timeout?

          OpenTelemetry.logger.warn("Resource detector #{result.detector} did not return within #{@resource_detection_timeout}s; its attributes were ignored")
        end

        detected = @resource_detection_results.map { |result| result.success? ? result.resource : nil }
        @detected_resource = Resources::Resource.merge_all(detected.compact)
        @resource = assemble_resource
      end

      def run_resource_detectors(detectors)
        return [] if detectors.empty?

        deadline = @resource_detection_timeout && OpenTelemetry::Common::Utilities.timeout_timestamp + @resource_detection_timeout
        worker_count = [@resource_detector_concurrency, detectors.length].min
        state = DetectionState.new(detectors.length, worker_count)
        queue = Queue.new
        detectors.each_with_index { |detector, index| queue << [index, detector] }
        spawn_detector_workers(worker_count, queue, state)
        state.finish(detectors, deadline)
      end

      def spawn_detector_workers(count, queue, state)
        Array.new(count) do
          Thread.new do
            Thread.current.report_on_exception = false
            drain_detectors(queue, state)
          ensure
            state.worker_finished
          end
        end
      end

      def drain_detectors(queue, state)
        while state.accepting? && (item = next_detector(queue))
          index, detector = item
          state.record(index, run_resource_detector(detector))
        end
      end

      def next_detector(queue)
        queue.pop(true)
      rescue ThreadError
        nil
      end

      # Detector failures are contained: they are reported in the results and
      # never abort SDK configuration.
      def run_resource_detector(detector)
        resource = coerce_detected_resource(detector.detect)
        DetectorResult.new(detector: detector, status: :success, resource: resource)
      rescue StandardError => e
        OpenTelemetry.logger.warn("Resource detector #{detector} failed: #{e.class}: #{e.message}")
        DetectorResult.new(detector: detector, status: :error, error: e)
      end

      def coerce_detected_resource(result)
        case result
        when Resources::Resource then result
        when Hash then Resources::Resource.create(result)
        when nil then Resources::Resource.create
        else raise TypeError, "#{result.class} is not a valid resource detector result"
        end
      end

      def assemble_resource
        @default_resource.merge(@detected_resource).merge(@user_resource)
      end

      def check_use_mode!(mode)
        @use_mode = mode if @use_mode == USE_MODE_UNSPECIFIED
        raise 'Use either `use_all` or `use`, but not both' unless @use_mode == mode
      end

      def install_instrumentation
        case @use_mode
        when USE_MODE_ONE
          OpenTelemetry::Instrumentation.registry.install(@instrumentation_names, @instrumentation_config_map)
        when USE_MODE_ALL
          OpenTelemetry::Instrumentation.registry.install_all(@instrumentation_config_map)
        end
      end

      def configure_span_processors
        processors = @span_processors.empty? ? wrapped_exporters_from_env.compact : @span_processors
        processors.each { |p| tracer_provider.add_span_processor(p) }
      end

      def wrapped_exporters_from_env # rubocop:disable Metrics/CyclomaticComplexity
        exporters = ENV.fetch('OTEL_TRACES_EXPORTER', 'otlp')
        exporters.split(',').map do |exporter|
          case exporter.strip
          when 'none' then nil
          when 'otlp'
            otlp_protocol = ENV['OTEL_EXPORTER_OTLP_TRACES_PROTOCOL'] || ENV['OTEL_EXPORTER_OTLP_PROTOCOL'] || 'http/protobuf'

            if otlp_protocol == 'http/protobuf'
              fetch_exporter(exporter, 'OpenTelemetry::Exporter::OTLP::Exporter')
            else
              OpenTelemetry.logger.warn "The #{otlp_protocol} transport protocol is not supported by the OTLP exporter, spans will not be exported."
              nil
            end
          when 'jaeger' then fetch_exporter(exporter, 'OpenTelemetry::Exporter::Jaeger::CollectorExporter')
          when 'zipkin' then fetch_exporter(exporter, 'OpenTelemetry::Exporter::Zipkin::Exporter')
          when 'console' then Trace::Export::SimpleSpanProcessor.new(Trace::Export::ConsoleSpanExporter.new)
          else
            OpenTelemetry.logger.warn "The #{exporter} exporter is unknown and cannot be configured, spans will not be exported"
            nil
          end
        end
      end

      def configure_propagation # rubocop:disable Metrics/CyclomaticComplexity
        propagators = ENV.fetch('OTEL_PROPAGATORS', 'tracecontext,baggage').split(',').uniq.collect do |propagator|
          case propagator
          when 'tracecontext' then OpenTelemetry::Trace::Propagation::TraceContext.text_map_propagator
          when 'baggage' then OpenTelemetry::Baggage::Propagation.text_map_propagator
          when 'b3' then fetch_propagator(propagator, 'OpenTelemetry::Propagator::B3::Single')
          when 'b3multi' then fetch_propagator(propagator, 'OpenTelemetry::Propagator::B3::Multi', 'b3')
          when 'jaeger' then fetch_propagator(propagator, 'OpenTelemetry::Propagator::Jaeger')
          when 'xray' then fetch_propagator(propagator, 'OpenTelemetry::Propagator::XRay')
          when 'ottrace' then fetch_propagator(propagator, 'OpenTelemetry::Propagator::OTTrace')
          when 'none' then NoopTextMapPropagator.new
          else
            OpenTelemetry.logger.warn "The #{propagator} propagator is unknown and cannot be configured"
            NoopTextMapPropagator.new
          end
        end
        OpenTelemetry.propagation = Context::Propagation::CompositeTextMapPropagator.compose_propagators((@propagators || propagators).compact)
      end

      def fetch_propagator(name, class_name, gem_suffix = name)
        Kernel.const_get(class_name).text_map_propagator
      rescue NameError
        OpenTelemetry.logger.warn "The #{name} propagator cannot be configured - please add opentelemetry-propagator-#{gem_suffix} to your Gemfile"
        nil
      end

      def fetch_exporter(name, class_name)
        Trace::Export::BatchSpanProcessor.new(Kernel.const_get(class_name).new)
      rescue NameError
        OpenTelemetry.logger.warn "The #{name} exporter cannot be configured - please add opentelemetry-exporter-#{name} to your Gemfile, spans will not be exported"
        nil
      end
    end
  end
end
