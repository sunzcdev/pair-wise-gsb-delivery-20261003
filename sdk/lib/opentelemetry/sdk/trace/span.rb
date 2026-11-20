# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Trace
      # Implementation of {OpenTelemetry::Trace::Span} that records trace events.
      #
      # This implementation includes reader methods intended to allow access to
      # internal state by {SpanProcessor}s.
      # Instrumentation should use the API provided by {OpenTelemetry::Trace::Span}
      # and should consider {Span} to be write-only.
      #
      # rubocop:disable-next Metrics/ClassLength
      class Span < OpenTelemetry::Trace::Span
        DEFAULT_STATUS = OpenTelemetry::Trace::Status.unset
        EMPTY_ATTRIBUTES = {}.freeze
        BASE_EXCEPTION_ATTRIBUTE_COUNT = 3

        private_constant :DEFAULT_STATUS, :EMPTY_ATTRIBUTES, :BASE_EXCEPTION_ATTRIBUTE_COUNT

        # The following readers are intended for the use of SpanProcessors and
        # should not be considered part of the public interface for instrumentation.
        attr_reader :name, :status, :kind, :parent_span_id, :start_timestamp, :end_timestamp, :links, :resource, :instrumentation_scope

        # Returns an InstrumentationScope struct, which is backwards compatible with InstrumentationLibrary.
        # @deprecated Please use instrumentation_scope instead.
        #
        # @return InstrumentationScope
        alias instrumentation_library instrumentation_scope

        # Return a frozen copy of the current attributes. This is intended for
        # use of SpanProcessors and should not be considered part of the public
        # interface for instrumentation.
        #
        # @return [Hash{String => String, Numeric, Boolean, Array<String, Numeric, Boolean>}] may be nil.
        def attributes
          # Don't bother synchronizing. Access by SpanProcessors is expected to
          # be serialized.
          @attributes&.clone.freeze
        end

        # Return a frozen copy of the current events. This is intended for use
        # of SpanProcessors and should not be considered part of the public
        # interface for instrumentation.
        #
        # @return [Array<Event>] may be nil.
        def events
          # Don't bother synchronizing. Access by SpanProcessors is expected to
          # be serialized.
          @events&.clone.freeze
        end

        # Return the flag whether this span is recording events
        #
        # @return [Boolean] true if this Span is active and recording information
        #   like events with the #add_event operation and attributes using
        #   #set_attribute.
        def recording?
          !@ended
        end

        # Set attribute
        #
        # Note that the OpenTelemetry project
        # {https://github.com/open-telemetry/opentelemetry-specification/blob/master/specification/data-semantic-conventions.md
        # documents} certain "standard attributes" that have prescribed semantic
        # meanings.
        #
        # @param [String] key
        # @param [String, Boolean, Numeric, Array<String, Numeric, Boolean>] value
        #   Values must be non-nil and (array of) string, boolean or numeric type.
        #   Array values must not contain nil elements and all elements must be of
        #   the same basic type (string, numeric, boolean).
        #
        # @return [self] returns itself
        def set_attribute(key, value)
          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling set_attribute on an ended Span.')
            else
              @attributes ||= {}
              @attributes[key] = value
              trim_span_attributes(@attributes)
              @total_recorded_attributes += 1
            end
          end
          self
        end
        alias []= set_attribute

        # Add attributes
        #
        # Note that the OpenTelemetry project
        # {https://github.com/open-telemetry/opentelemetry-specification/blob/master/specification/data-semantic-conventions.md
        # documents} certain "standard attributes" that have prescribed semantic
        # meanings.
        #
        # @param [Hash{String => String, Numeric, Boolean, Array<String, Numeric, Boolean>}] attributes
        #   Values must be non-nil and (array of) string, boolean or numeric type.
        #   Array values must not contain nil elements and all elements must be of
        #   the same basic type (string, numeric, boolean).
        #
        # @return [self] returns itself
        def add_attributes(attributes)
          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling add_attributes on an ended Span.')
            else
              @attributes ||= {}
              @attributes.merge!(attributes)
              trim_span_attributes(@attributes)
              @total_recorded_attributes += attributes.size
            end
          end
          self
        end

        # Add a link to a {Span}.
        #
        # Adding links at span creation using the `links` option is preferred
        # to calling add_link later, because head sampling decisions can only
        # consider information present during span creation.
        #
        # Example:
        #
        #   span.add_link(OpenTelemetry::Trace::Link.new(span_to_link_from.context))
        #
        # Note that the OpenTelemetry project
        # {https://github.com/open-telemetry/opentelemetry-specification/blob/master/specification/data-semantic-conventions.md
        # documents} certain "standard attributes" that have prescribed semantic
        # meanings.
        #
        # @param [OpenTelemetry::Trace::Link] link The link object to add on the {Span}.
        #
        # @return [self] returns itself
        def add_link(link)
          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling add_link on an ended Span.')
            else
              @links ||= []
              @links = trim_links(@links << link, @span_limits.link_count_limit, @span_limits.link_attribute_count_limit)
              @total_recorded_links += 1
            end
          end
          self
        end

        # Add an Event to a {Span}.
        #
        # Example:
        #
        #   span.add_event('event', attributes: {'eager' => true})
        #
        # Note that the OpenTelemetry project
        # {https://github.com/open-telemetry/opentelemetry-specification/blob/master/specification/data-semantic-conventions.md
        # documents} certain "standard event names and keys" which have
        # prescribed semantic meanings.
        #
        # @param [String] name Name of the event.
        # @param [optional Hash{String => String, Numeric, Boolean, Array<String, Numeric, Boolean>}] attributes
        #   One or more key:value pairs, where the keys must be strings and the
        #   values may be (array of) string, boolean or numeric type.
        # @param [optional Time] timestamp Optional timestamp for the event.
        #
        # @return [self] returns itself
        def add_event(name, attributes: nil, timestamp: nil)
          event = Event.new(name, truncate_attribute_values(attributes, @span_limits.event_attribute_length_limit), relative_timestamp(timestamp))

          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling add_event on an ended Span.')
            else
              @events ||= []
              @events = append_event(@events, event)
              @total_recorded_events += 1
            end
          end
          self
        end

        # Record an exception during the execution of this span. Multiple exceptions
        # can be recorded on a span.
        #
        # When `record_cause_chain` is true, the exception's `cause` chain is
        # recorded alongside the event as a bounded value snapshot. Each recorded
        # cause carries its depth (`exception.cause.<depth>.depth`, where the
        # recorded exception itself is depth 0), its own frames, and the number of
        # trailing frames it shares with the exception wrapping it
        # (`exception.cause.<depth>.shared_frames`). Shared frames are not repeated
        # in `exception.cause.<depth>.stacktrace`, so the frames recorded there
        # locate the point where that cause diverged from its wrapper.
        #
        # `exception.cause.count` reports how many causes were recorded. When the
        # chain could not be recorded in full, `exception.cause.truncated` explains
        # where it stopped: a cause count limit, a message or stacktrace length
        # limit, or an exception reference that was already recorded
        # (`exception.cause.<depth>.alias_of` names the earlier depth).
        #
        # @param [Exception] exception The exception to be recorded
        # @param [optional Hash{String => String, Numeric, Boolean, Array<String, Numeric, Boolean>}]
        #   attributes One or more key:value pairs, where the keys must be
        #   strings and the values may be (array of) string, boolean or numeric
        #   type.
        # @param [optional Boolean] record_cause_chain Whether to record the
        #   exception's cause chain. Defaults to false.
        #
        # @return [void]
        def record_exception(exception, attributes: nil, record_cause_chain: false)
          event_attributes = {
            'exception.type' => exception.class.to_s,
            'exception.message' => exception.message,
            'exception.stacktrace' => utf8_scrub(exception.full_message(highlight: false, order: :top))
          }
          event_attributes.merge!(cause_chain_attributes(exception, attributes&.size || 0)) if record_cause_chain
          event_attributes.merge!(attributes) unless attributes.nil?
          add_event('exception', attributes: event_attributes)
        end

        # Sets the Status to the Span
        #
        # If used, this will override the default Span status. Default has code = Status::UNSET.
        #
        # An attempt to set the status with code == Status::UNSET is ignored.
        # If the status is set with code == Status::OK, any further attempt to set the status
        # is ignored.
        #
        # @param [Status] status The new status, which overrides the default Span
        #   status, which has code = Status::UNSET.
        #
        # @return [void]
        def status=(status)
          return if status.code == OpenTelemetry::Trace::Status::UNSET

          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling status= on an ended Span.')
            elsif @status.code != OpenTelemetry::Trace::Status::OK
              @status = status
            end
          end
        end

        # Updates the Span name
        #
        # Upon this update, any sampling behavior based on Span name will depend
        # on the implementation.
        #
        # @param [String] new_name The new operation name, which supersedes
        #   whatever was passed in when the Span was started
        #
        # @return [void]
        def name=(new_name)
          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling name= on an ended Span.')
            else
              @name = new_name
            end
          end
        end

        # Finishes the Span
        #
        # Implementations MUST ignore all subsequent calls to {#finish} (there
        # might be exceptions when Tracer is streaming event and has no mutable
        # state associated with the Span).
        #
        # Call to {#finish} MUST not have any effects on child spans. Those may
        # still be running and can be ended later.
        #
        # This API MUST be non-blocking*.
        #
        # (*) not actually non-blocking. In particular, it synchronizes on an
        # internal mutex, which will typically be uncontended, and
        # {Export::BatchSpanProcessor} will also synchronize on a mutex, if that
        # processor is used.
        #
        # @param [Time] end_timestamp optional end timestamp for the span.
        #
        # @return [self] returns itself
        def finish(end_timestamp: nil)
          @mutex.synchronize do
            if @ended
              OpenTelemetry.logger.warn('Calling finish on an ended Span.')
              return self
            end
            @end_timestamp = relative_timestamp(end_timestamp)
            @span_processors.each do |processor|
              processor.on_finishing(self) if processor.respond_to?(:on_finishing)
            end
            @attributes = validated_attributes(@attributes).freeze
            @events.freeze
            @links.freeze
            @ended = true
          end
          @span_processors.each { |processor| processor.on_finish(self) }
          self
        end

        # @api private
        #
        # Returns a SpanData containing a snapshot of the Span fields. It is
        # assumed that the Span has been finished, and that no further
        # modifications will be made to the Span.
        #
        # This method should be called *only* from a SpanProcessor prior to
        # calling the SpanExporter.
        #
        # @return [SpanData]
        def to_span_data
          SpanData.new(
            @name,
            @kind,
            @status,
            @parent_span_id,
            @total_recorded_attributes,
            @total_recorded_events,
            @total_recorded_links,
            @start_timestamp,
            @end_timestamp,
            @attributes,
            @links,
            @events,
            @resource,
            @instrumentation_scope,
            context.span_id,
            context.trace_id,
            context.trace_flags,
            context.tracestate,
            @parent_span_is_remote
          )
        end

        # @api private
        def initialize(context, parent_context, parent_span, name, kind, parent_span_id, span_limits, span_processors, attributes, links, start_timestamp, resource, instrumentation_scope) # rubocop:disable Metrics/MethodLength, Metrics/PerceivedComplexity, Metrics/CyclomaticComplexity
          super(span_context: context)
          @mutex = Mutex.new
          @name = name
          @kind = kind
          @parent_span_id = parent_span_id.freeze || OpenTelemetry::Trace::INVALID_SPAN_ID
          @parent_span_is_remote = parent_span&.context&.remote? || false
          @span_limits = span_limits
          @span_processors = span_processors
          @resource = resource
          @instrumentation_scope = instrumentation_scope
          @ended = false
          @status = DEFAULT_STATUS
          @total_recorded_events = 0
          @total_recorded_links = links&.size || 0
          @total_recorded_attributes = attributes&.size || 0
          @attributes = attributes
          trim_span_attributes(@attributes)
          @events = nil
          @links = trim_links(links, span_limits.link_count_limit, span_limits.link_attribute_count_limit)

          # Times are hard. Whenever an explicit timestamp is provided
          # (for Events or for the Span start_timestamp or end_timestamp),
          # we use that as the recorded timestamp. An implicit Event timestamp
          # and end_timestamp is computed as a monotonic clock offset from
          # the realtime start_timestamp. The realtime start_timestamp is
          # computed as a monotonic clock offset from the realtime
          # start_timestamp of its parent span, if available, or it is
          # fetched from the realtime system clock.
          #
          # We therefore have 3 start timestamps. The first two are used
          # internally (and by child spans) to compute other timestamps.
          # The last is the start timestamp actually recorded in the
          # SpanData.
          @monotonic_start_timestamp = monotonic_now
          @realtime_start_timestamp = if parent_span.recording?
                                        relative_realtime(parent_span.realtime_start_timestamp, parent_span.monotonic_start_timestamp, @monotonic_start_timestamp)
                                      else
                                        realtime_now
                                      end
          @start_timestamp = if start_timestamp
                               time_in_nanoseconds(start_timestamp)
                             else
                               @realtime_start_timestamp
                             end
          @end_timestamp = nil
          @span_processors.each { |processor| processor.on_start(self, parent_context) }
        end

        # TODO: Java implementation overrides finalize to log if a span isn't finished.

        protected

        attr_reader :monotonic_start_timestamp, :realtime_start_timestamp

        private

        # Attributes describing the `cause` chain of an exception, bounded by the
        # span's exception cause limits and by the event attribute budget.
        def cause_chain_attributes(exception, extra_attribute_count)
          # Space left for the chain once the base exception attributes and the
          # caller's attributes are accounted for.
          remaining = @span_limits.event_attribute_count_limit - BASE_EXCEPTION_ATTRIBUTE_COUNT - extra_attribute_count
          walk = { attributes: {}, truncated: [], seen: [exception], budget: remaining - 2, bookkeeping: bookkeeping_slots(remaining) }
          walk_cause_chain(walk, exception) if remaining >= 1
          finish_cause_chain(walk)
        rescue StandardError => e
          OpenTelemetry.handle_error(exception: e, message: 'error recording exception cause chain')
          # Keep whatever was gathered before the failure; the marker explains
          # that the truncation was unexpected rather than a configured limit.
          walk ||= { attributes: {}, truncated: [], seen: nil, bookkeeping: %i[count truncated] }
          walk[:truncated] << 'error'
          finish_cause_chain(walk)
        end

        def walk_cause_chain(walk, current)
          while (cause = current.cause)
            depth = walk[:seen].size
            # A cause already recorded at an earlier depth means the chain loops;
            # point at the earlier depth instead of walking it again.
            if (aliased_depth = aliased_depth_of(walk[:seen], cause))
              walk[:truncated] << 'alias'
              add_aliased_cause(walk, cause, depth, aliased_depth)
              return
            end
            if depth > @span_limits.exception_cause_count_limit
              walk[:truncated] << 'cause_count_limit'
              return
            end
            unless record_cause(walk, cause, depth, current)
              walk[:truncated] << 'attribute_count_limit'
              return
            end

            current = cause
          end
        end

        # Records one cause only when its whole description fits the remaining
        # budget; a cause that can say less than its type and message is not
        # worth counting as recorded.
        def record_cause(walk, cause, depth, wrapper)
          values, length_truncated = cause_attributes(cause, depth, wrapper)
          return false if walk[:budget] < values.size

          walk[:attributes].merge!(values)
          walk[:budget] -= values.size
          walk[:seen] << cause
          walk[:truncated] << 'length_limit' unless length_truncated.empty?
          true
        end

        def finish_cause_chain(walk)
          slots = walk[:bookkeeping]
          # `exception.cause.count` outranks the truncation marker: a reader
          # almost always wants to know how many causes were captured.
          walk[:attributes]['exception.cause.count'] = [walk[:seen].size - 1, 0].max if walk[:seen] && slots.include?(:count)
          if slots.include?(:truncated) && !walk[:truncated].empty?
            walk[:attributes]['exception.cause.truncated'] = walk[:truncated].uniq.join(',')
          end
          walk[:attributes]
        end

        def bookkeeping_slots(remaining)
          # Reserve the bookkeeping attributes last, so a very tight budget still
          # reports how many causes were captured even when nothing else fits.
          case remaining
          when 0 then []
          when 1 then [:count]
          else %i[count truncated]
          end
        end

        def aliased_depth_of(seen, cause)
          seen.index { |seen_exception| seen_exception.equal?(cause) }
        end

        def add_aliased_cause(walk, cause, depth, aliased_depth)
          # The pointer back to the earlier depth is what a reader needs most, so
          # it is recorded before the descriptive fields, which are dropped when
          # the event attribute budget cannot fit them.
          fields = [['alias_of', aliased_depth], ['depth', depth], ['type', cause.class.to_s]]
          recorded = fields.first([walk[:budget], 0].max).to_h { |name, value| [cause_key(depth, name), value] }
          walk[:truncated] << 'attribute_count_limit' if recorded.size < fields.size
          walk[:attributes].merge!(recorded)
          walk[:budget] -= recorded.size
        end

        # Builds the attributes describing one cause. Returns the attributes and
        # which values a length limit shortened.
        def cause_attributes(cause, depth, wrapper)
          length_truncated = []
          fields = cause_fields(cause, depth, wrapper, length_truncated)
          fields << ['truncated', length_truncated.uniq.join(',')] unless length_truncated.empty?
          [fields.to_h { |name, value| [cause_key(depth, name), value] }, length_truncated]
        end

        def cause_fields(cause, depth, wrapper, length_truncated)
          message = cause_message(cause, length_truncated)
          backtrace = cause.backtrace || []
          shared_frames = shared_frame_count(backtrace, wrapper.backtrace || [])
          # The boundary starts where the two exceptions share frames, so the
          # recorded prefix stops exactly where the wrapping exception takes over.
          stacktrace = cause_stacktrace(backtrace.first(backtrace.size - shared_frames), length_truncated)

          fields = [['type', cause.class.to_s], ['depth', depth], ['message', message], ['shared_frames', shared_frames]]
          fields << ['stacktrace', stacktrace] unless stacktrace.empty?
          fields
        end

        def cause_message(cause, length_truncated)
          message = utf8_scrub(cause.message.to_s)
          return message unless over_length_limit?(message)

          length_truncated << 'message'
          OpenTelemetry::Common::Utilities.truncate(message, cause_length_limit)
        end

        def cause_stacktrace(frames, length_truncated)
          stacktrace = frames.map { |frame| utf8_scrub(frame.to_s) }.join("\n")
          return stacktrace unless over_length_limit?(stacktrace)

          length_truncated << 'stacktrace'
          OpenTelemetry::Common::Utilities.truncate(stacktrace, cause_length_limit)
        end

        def over_length_limit?(value)
          !cause_length_limit.nil? && value.size > cause_length_limit
        end

        def cause_key(depth, name)
          "exception.cause.#{depth}.#{name}"
        end

        def shared_frame_count(frames, wrapper_frames)
          limit = [frames.size, wrapper_frames.size].min
          count = 0
          count += 1 while count < limit && frames[-1 - count] == wrapper_frames[-1 - count]
          count
        end

        def cause_length_limit
          # Cause values pass through the same truncation as any other event
          # attribute, so the tighter limit is the one worth reporting against.
          [@span_limits.exception_cause_length_limit, @span_limits.event_attribute_length_limit].compact.min
        end

        def utf8_scrub(string)
          string.encode('UTF-8', invalid: :replace, undef: :replace, replace: '�')
        end

        def validated_attributes(attrs)
          return attrs if Internal.valid_attributes?(name, 'span', attrs)

          attrs.keep_if { |key, value| Internal.valid_key?(key) && Internal.valid_value?(value) }
        end

        def trim_span_attributes(attrs)
          return if attrs.nil?

          if attrs.size > @span_limits.attribute_count_limit
            n = @span_limits.attribute_count_limit
            attrs.delete_if do |_key, _value|
              n -= 1
              n.negative?
            end
          end

          truncate_attribute_values(attrs, @span_limits.attribute_length_limit)
          nil
        end

        def truncate_attribute_values(attrs, attribute_length_limit)
          return EMPTY_ATTRIBUTES if attrs.nil?
          return attrs if attribute_length_limit.nil?

          attrs.transform_values! { |value| OpenTelemetry::Common::Utilities.truncate_attribute_value(value, attribute_length_limit) }
          attrs
        end

        def trim_links(links, link_count_limit, link_attribute_count_limit) # rubocop:disable Metrics/CyclomaticComplexity, Metrics/PerceivedComplexity
          # Fast path (likely) common cases.
          return nil if links.nil?

          if links.size <= link_count_limit &&
             links.all? { |link| link.span_context.valid? && link.attributes.size <= link_attribute_count_limit && Internal.valid_attributes?(name, 'link', link.attributes) }
            return links
          end

          # Slow path: trim attributes for each Link.
          valid_links = links.select { |link| link.span_context.valid? }
          excess_link_count = valid_links.size - link_count_limit
          valid_links.pop(excess_link_count) if excess_link_count.positive?
          valid_links.map! do |link|
            attrs = link.attributes.to_h.dup # link.attributes is frozen, so we need an unfrozen copy to adjust.
            attrs.keep_if { |key, value| Internal.valid_key?(key) && Internal.valid_value?(value) }
            excess = attrs.size - link_attribute_count_limit
            excess.times { attrs.shift } if excess.positive?
            OpenTelemetry::Trace::Link.new(link.span_context, attrs)
          end
        end

        def append_event(events, event) # rubocop:disable Metrics/CyclomaticComplexity, Metrics/PerceivedComplexity
          event_count_limit = @span_limits.event_count_limit
          event_attribute_count_limit = @span_limits.event_attribute_count_limit
          valid_attributes = Internal.valid_attributes?(name, 'event', event.attributes)

          # Fast path (likely) common case.
          if events.size < event_count_limit &&
             event.attributes.size <= event_attribute_count_limit &&
             valid_attributes
            return events << event
          end

          # Slow path.
          excess = events.size + 1 - event_count_limit
          events.shift(excess) if excess.positive?

          excess = event.attributes.size - event_attribute_count_limit
          if excess.positive? || !valid_attributes
            attrs = event.attributes.to_h.dup # event.attributes is frozen, so we need an unfrozen copy to adjust.
            attrs.keep_if { |key, value| Internal.valid_key?(key) && Internal.valid_value?(value) }
            excess = attrs.size - event_attribute_count_limit
            excess.times { attrs.shift } if excess.positive?
            event = Event.new(event.name, attrs.freeze, event.timestamp)
          end
          events << event
        end

        def relative_timestamp(timestamp)
          return time_in_nanoseconds(timestamp) unless timestamp.nil?

          relative_realtime(realtime_start_timestamp, monotonic_start_timestamp, monotonic_now)
        end

        def time_in_nanoseconds(timestamp)
          (timestamp.to_r * 1_000_000_000).to_i
        end

        def relative_realtime(realtime_base, monotonic_base, now)
          realtime_base + (now - monotonic_base)
        end

        def realtime_now
          Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond)
        end

        def monotonic_now
          Process.clock_gettime(Process::CLOCK_MONOTONIC, :nanosecond)
        end
      end
    end
  end
end
