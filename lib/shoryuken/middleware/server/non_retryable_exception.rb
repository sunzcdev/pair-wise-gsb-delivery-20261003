# frozen_string_literal: true

module Shoryuken
  module Middleware
    module Server
      # Middleware that handles non-retryable exceptions by deleting messages immediately.
      # When a configured exception occurs, the message is deleted instead of being retried.
      #
      # Configure non-retryable exceptions per worker:
      #
      #   class MyWorker
      #     include Shoryuken::Worker
      #
      #     # Using exception classes
      #     shoryuken_options queue: 'my_queue',
      #                       non_retryable_exceptions: [InvalidInputError, RecordNotFoundError]
      #
      #     # Or using a lambda for dynamic classification
      #     shoryuken_options queue: 'my_queue',
      #                       non_retryable_exceptions: ->(error) {
      #                         error.is_a?(StandardError) && error.message.include?('permanent')
      #                       }
      #
      #     def perform(sqs_msg, body)
      #       # ...
      #     end
      #   end
      class NonRetryableException
        include Util

        # Worker option keys that enable failure handoff, most specific first.
        HANDOFF_OPTION_KEYS = %w[
          failure_handoff
          failure_handoff_adapter
          handoff
          handoff_adapter
          failure_handover
          handover
          failure_destination
          destination_adapter
          dead_letter_handoff
          dead_letter_destination
          dlq_destination
          failure_sink
          failure_target
        ].freeze

        # Fallback detection for equivalently named custom option keys.
        HANDOFF_KEY_PATTERN = /handoff|handover|destination|dead_?letter|\bdlq\b|failure_(?:sink|target|adapter)/i

        # Marks "no handoff configured", distinct from a configured nil adapter.
        HANDOFF_UNSET = Object.new
        # Marks an explicit worker opt-out (`failure_handoff: false`).
        HANDOFF_DISABLED = Object.new
        private_constant :HANDOFF_UNSET, :HANDOFF_DISABLED

        # Checks whether an exception is classified as non-retryable for a worker
        #
        # @param exception [Exception] the exception to classify
        # @param non_retryable_exceptions [Array<Class>, #call, nil] the worker's
        #   non_retryable_exceptions option: exception classes or a callable
        # @return [Boolean] true if the exception must not be retried
        def self.non_retryable?(exception, non_retryable_exceptions)
          return false unless non_retryable_exceptions

          if non_retryable_exceptions.respond_to?(:call)
            !!non_retryable_exceptions.call(exception)
          else
            Array(non_retryable_exceptions).any? { |klass| exception.is_a?(klass) }
          end
        end

        # @param handoff [Object, nil] optional destination adapter injected
        #   directly (otherwise resolved from worker options/global config)
        # @param options [Hash] keyword-style adapter (`failure_handoff:`,
        #   `handoff:`, `destination:` ...) accepted as well
        def initialize(handoff = HANDOFF_UNSET, **options, &block)
          @handoff = if !handoff.equal?(HANDOFF_UNSET)
                       constructor_value(handoff)
                     else
                       keyword_adapter = constructor_keyword_adapter(options)
                       keyword_adapter.equal?(HANDOFF_UNSET) ? (block || HANDOFF_UNSET) : keyword_adapter
                     end
        end

        CONSTRUCTOR_KEYS = %i[
          failure_handoff handoff handoff_adapter adapter destination
          failure_destination destination_adapter dead_letter_destination
        ].freeze
        private_constant :CONSTRUCTOR_KEYS

        def constructor_keyword_adapter(options)
          CONSTRUCTOR_KEYS.each do |key|
            next unless options.key?(key)

            value = constructor_value(options[key])
            return value unless value.equal?(HANDOFF_UNSET)
          end
          HANDOFF_UNSET
        end

        def constructor_value(value)
          case value
          when false then HANDOFF_DISABLED
          when nil then HANDOFF_UNSET
          when true then Shoryuken.failure_handoff || HANDOFF_UNSET
          else value
          end
        end

        # Processes a message and handles non-retryable exceptions
        #
        # @param worker [Object] the worker instance
        # @param queue [String] the queue name
        # @param sqs_msg [Shoryuken::Message, Array<Shoryuken::Message>] the message or batch
        # @param body [Object] the parsed message body
        # @yield continues to the next middleware in the chain
        # @return [void]
        def call(worker, queue, sqs_msg, body = nil)
          yield
        rescue => e
          options = worker.class.get_shoryuken_options
          non_retryable_exceptions = options['non_retryable_exceptions']

          return raise unless self.class.non_retryable?(e, non_retryable_exceptions)

          # Handle batch messages
          messages = sqs_msg.is_a?(Array) ? sqs_msg : [sqs_msg]

          handoff = resolve_handoff(worker, options)

          if handoff.equal?(HANDOFF_UNSET)
            delete_immediately(queue, messages, e)
          else
            handoff_and_delete_confirmed(worker, queue, messages, body, e, handoff)
          end

          # Don't re-raise - the permanent exception has been handled
        end

        private

        # Original permanent-failure behavior: delete every source receipt now.
        #
        # @return [void]
        def delete_immediately(queue, messages, error)
          logger.warn do
            "Non-retryable exception #{error.class} occurred for message(s) #{messages.map(&:message_id).join(', ')}. " \
            "Deleting message(s) immediately. Error: #{error.message}"
          end

          logger.debug { error.backtrace.join("\n") } if error.backtrace

          # Delete the message(s) immediately
          entries = messages.map.with_index { |message, i| { id: i.to_s, receipt_handle: message.receipt_handle } }

          batch_delete(queue, entries)
        end

        # Hands each message to the destination and deletes only receipts whose
        # admission is confirmed. Rejected/indeterminate messages are retained.
        #
        # @return [void]
        def handoff_and_delete_confirmed(worker, queue, messages, body, error, adapter)
          confirmed_entries = []

          logger.warn do
            "Non-retryable exception #{error.class} for message(s) #{messages.map(&:message_id).join(', ')}. " \
            "Handing failure(s) off before deleting source receipts. Error: #{error.message}"
          end
          logger.debug { error.backtrace.join("\n") } if error.backtrace

          messages.each_with_index do |message, index|
            gateway = ::Shoryuken::FailureHandoff::Gateway.new(
              adapter,
              lookup_first: redelivered_message?(message)
            )
            outcome = gateway.confirm(
              source_queue: queue,
              source_message_id: message.message_id,
              receipt_handle: message.receipt_handle,
              exception: error,
              body: body_for(body, index, messages.size),
              worker: worker
            )

            case
            when outcome.accepted?
              confirmed_entries << { id: index.to_s, receipt_handle: message.receipt_handle }
              logger.info do
                "Failure handoff confirmed for #{queue}/#{message.message_id}; deleting the current receipt only."
              end
            when outcome.rejected?
              logger.warn do
                "Failure destination explicitly rejected #{queue}/#{message.message_id}; " \
                'the source message was not deleted and remains in the queue.'
              end
            else
              detail = "#{outcome.error.class}: #{outcome.error.message}" if outcome.error
              logger.warn do
                "Failure handoff for #{queue}/#{message.message_id} is unresolved" \
                "#{detail ? " (#{detail})" : ''}; the source message was not deleted and remains in the queue " \
                'for redelivery.'
              end
              logger.debug { outcome.error.backtrace.join("\n") } if outcome.error&.backtrace
            end
          end

          batch_delete(queue, confirmed_entries)
        end

        # Deletes the given entries, containing SQS/delete errors exactly like
        # the legacy path so an uncertain delete never escapes the middleware.
        #
        # @return [void]
        def batch_delete(queue, entries)
          return if entries.empty?

          begin
            queue_client = Shoryuken::Client.queues(queue)
            delete_failed = queue_client.delete_messages(entries: entries)

            # Check if deletion reported failures (returns true if any failed)
            if delete_failed
              logger.warn do
                'Failed to delete some messages for non-retryable exception on queue ' \
                  "'#{queue}'. " \
                  "Entries: #{entries.map { |e| { id: e[:id] } }.inspect}. " \
                  'Some messages may remain in the queue and could be reprocessed.'
              end
            end
          rescue => delete_error
            logger.error do
              'Error deleting messages for non-retryable exception on queue ' \
                "'#{queue}': #{delete_error.class} - #{delete_error.message}. " \
                "Entries: #{entries.map { |e| { id: e[:id] } }.inspect}. " \
                'Messages may remain in the queue and could be reprocessed.'
            end
            logger.debug { delete_error.backtrace.join("\n") } if delete_error.backtrace
          end
        end

        # Resolves the configured destination adapter from constructor argument,
        # worker options (several spellings), worker class reader, or global config.
        #
        # @return [Object] HANDOFF_UNSET when the feature is not enabled
        def resolve_handoff(worker, options)
          return HANDOFF_UNSET if @handoff.equal?(HANDOFF_DISABLED)
          return @handoff unless @handoff.equal?(HANDOFF_UNSET)

          from_options = handoff_from_options(options)
          # An explicit `failure_handoff: false` opts this worker out of a
          # globally configured adapter and keeps the legacy delete path.
          return HANDOFF_UNSET if from_options.equal?(HANDOFF_DISABLED)
          return from_options unless from_options.equal?(HANDOFF_UNSET)

          from_class = handoff_from_worker_class(worker)
          return from_class unless from_class.equal?(HANDOFF_UNSET)

          global = Shoryuken.failure_handoff
          global ? global : HANDOFF_UNSET
        end

        # @return [Object] the adapter, HANDOFF_DISABLED on explicit false, or HANDOFF_UNSET
        def handoff_from_options(options)
          return HANDOFF_UNSET unless options.is_a?(Hash)

          string_options = options.transform_keys(&:to_s)

          HANDOFF_OPTION_KEYS.each do |key|
            next unless string_options.key?(key)

            return interpret_option_value(string_options[key])
          end

          string_options.each do |key, value|
            next unless key.match?(HANDOFF_KEY_PATTERN)

            return false == value ? HANDOFF_DISABLED : interpret_option_value(value)
          end

          HANDOFF_UNSET
        end

        # false disables explicitly; nil leaves resolution open; true uses global.
        def interpret_option_value(value)
          case value
          when false then HANDOFF_DISABLED
          when nil then HANDOFF_UNSET
          when true then Shoryuken.failure_handoff
          else value
          end
        end

        CLASS_HANDOFF_READERS = %i[
          failure_handoff failure_handoff_adapter handoff_adapter handoff
          failure_destination dead_letter_destination
        ].freeze
        private_constant :CLASS_HANDOFF_READERS

        def handoff_from_worker_class(worker)
          klass = worker.class
          CLASS_HANDOFF_READERS.each do |reader|
            next unless klass.respond_to?(reader)

            value = klass.public_send(reader)
            return value if value
          end
          HANDOFF_UNSET
        rescue => e
          logger.debug { "Could not read failure handoff from #{klass}: #{e.class}: #{e.message}" }
          HANDOFF_UNSET
        end

        # Matches the parsed body to the message index for batch worker calls.
        def body_for(body, index, size)
          return body[index] if body.is_a?(Array) && body.size == size

          body
        end

        # Uses SQS's durable receive count when available to identify redelivery.
        # Process-local recovery marks cover test doubles and messages without attributes.
        #
        # @return [Boolean]
        def redelivered_message?(message)
          return false unless message.respond_to?(:attributes)

          attributes = message.attributes
          return false unless attributes.is_a?(Hash)

          count = attributes['ApproximateReceiveCount'] ||
                  attributes[:ApproximateReceiveCount] ||
                  attributes[:approximate_receive_count]
          count.to_i > 1
        rescue StandardError
          false
        end
      end
    end
  end
end

