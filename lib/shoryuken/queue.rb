# frozen_string_literal: true

module Shoryuken
  # Represents an SQS queue and provides methods for sending and receiving messages.
  # Handles both standard and FIFO queues, automatically adding required FIFO attributes.
  class Queue
    include Util

    # SQS attribute name for FIFO queue identification
    FIFO_ATTR               = 'FifoQueue'

    # Default message group ID used for FIFO queues
    MESSAGE_GROUP_ID        = 'ShoryukenMessage'

    # SQS attribute name for visibility timeout
    VISIBILITY_TIMEOUT_ATTR = 'VisibilityTimeout'

    # SQS attribute name for the dead-letter queue redrive policy
    REDRIVE_POLICY_ATTR     = 'RedrivePolicy'

    # Total batch delete attempts made by {#delete_messages_settlement}: the initial batch plus
    # one targeted redo containing only clearly retryable receipts.
    DELETE_MAX_ATTEMPTS     = 2

    # @return [String] the queue name
    attr_accessor :name

    # @return [Aws::SQS::Client] the SQS client
    attr_accessor :client

    # @return [String] the queue URL
    attr_accessor :url
    # Initializes a new Queue instance
    #
    # @param client [Aws::SQS::Client] the SQS client
    # @param name_or_url_or_arn [String] the queue name, URL, or ARN
    def initialize(client, name_or_url_or_arn)
      self.client = client
      set_name_and_url(name_or_url_or_arn)
    end

    # Returns the visibility timeout for the queue
    #
    # @return [Integer] the visibility timeout in seconds
    def visibility_timeout
      # Always lookup for the latest visibility when cache is disabled
      # setting it to nil, forces re-lookup
      @_visibility_timeout = nil unless Shoryuken.cache_visibility_timeout?
      @_visibility_timeout ||= queue_attributes.attributes[VISIBILITY_TIMEOUT_ATTR].to_i
    end

    # Deletes messages from the queue in batch
    #
    # @param options [Hash] options for delete_message_batch
    # @option options [Array<Hash>] :entries entries to delete with id and receipt_handle
    # @return [Boolean] true if any messages failed to delete
    def delete_messages(options)
      failed_messages = client.delete_message_batch(
        options.merge(queue_url: url)
      ).failed || []

      log_delete_failures(failed_messages)

      failed_messages.any?
    end

    # Deletes messages in batch and returns a grouped {DeletionResult} settlement.
    #
    # Unlike {#delete_messages}, this consumes the per-receipt +successful+/+failed+
    # entries of the batch response. Receipts whose failure code clearly names a
    # temporary broker condition are re-sent - on their own, without the already
    # confirmed or terminally rejected receipts - up to {DELETE_MAX_ATTEMPTS} total
    # attempts. Expired/invalid receipts such as +ReceiptHandleIsInvalid+ are never
    # retried. The worker handler is not involved here, so it never runs twice.
    #
    # Accepts either a positional options hash or keyword arguments:
    #
    #   queue.delete_messages_settlement(entries: entries)
    #   queue.delete_messages_settlement({ entries: entries }, max_attempts: 3)
    #
    # @param options [Hash, nil] options containing +:entries+
    # @param max_attempts [Integer] total attempts including the initial batch
    # @param extra [Hash] keyword-style options (e.g. +entries:+) when +options+ is omitted
    # @return [Shoryuken::DeletionResult] the final grouped settlement
    def delete_messages_settlement(options = nil, max_attempts: DELETE_MAX_ATTEMPTS, **extra)
      options = extra.merge(options || {})
      entries = Array(options[:entries] || options['entries'])
      # Only entries are re-sent per attempt; never leak the control key to the AWS request.
      request_options = options.reject do |key, _|
        %i[entries max_attempts].include?(key.to_sym)
      end
      max_attempts = [max_attempts.to_i, 1].max

      settlement = DeletionResult.new(entries)
      pending_entries = entries
      attempts = 0

      loop do
        response = client.delete_message_batch(request_options.merge(entries: pending_entries, queue_url: url))
        settlement.record(response)
        log_delete_failures(response_failures(response))

        attempts += 1
        pending_entries = settlement.retry_entries

        break if pending_entries.empty? || attempts >= max_attempts
      end

      settlement
    end
    alias delete_messages_result delete_messages_settlement
    alias delete_settlement delete_messages_settlement

    # Sends a single message to the queue
    #
    # @param options [Hash, String] message options or message body string
    # @option options [String] :message_body the message body
    # @option options [Integer] :delay_seconds delay before message becomes visible
    # @option options [Hash] :message_attributes custom message attributes
    # @option options [String] :message_group_id FIFO queue message group ID
    # @option options [String] :message_deduplication_id FIFO queue deduplication ID
    # @return [Aws::SQS::Types::SendMessageResult] the send result
    def send_message(options)
      options = sanitize_message!(options).merge(queue_url: url)

      Shoryuken.client_middleware.invoke(options) do
        client.send_message(options)
      end
    end

    # Sends multiple messages to the queue in batch
    #
    # @param options [Hash, Array] batch options or array of message bodies/hashes
    # @option options [Array<Hash>] :entries message entries to send
    # @return [Aws::SQS::Types::SendMessageBatchResult] the batch send result
    def send_messages(options)
      response = client.send_message_batch(sanitize_messages!(options).merge(queue_url: url))

      log_failed_sends(response)

      response
    end

    # Receives messages from the queue
    #
    # @param options [Hash] options for receive_message
    # @option options [Integer] :max_number_of_messages maximum messages to receive
    # @option options [Integer] :visibility_timeout visibility timeout for received messages
    # @option options [Integer] :wait_time_seconds long polling wait time
    # @option options [Array<String>] :attribute_names SQS attributes to retrieve
    # @option options [Array<String>] :message_attribute_names message attributes to retrieve
    # @return [Array<Shoryuken::Message>] the received messages
    def receive_messages(options)
      messages = client.receive_message(options.merge(queue_url: url)).messages || []
      messages.map { |m| Message.new(client, self, m) }
    end

    # Checks if the queue is a FIFO queue
    #
    # @return [Boolean] true if the queue is a FIFO queue
    def fifo?
      # Make sure the memoization work with boolean to avoid multiple calls to SQS
      # see https://github.com/ruby-shoryuken/shoryuken/pull/529
      return @_fifo if defined?(@_fifo)

      @_fifo = queue_attributes.attributes[FIFO_ATTR] == 'true'
      @_fifo
    end

    # Returns the number of receives after which SQS moves a message to the dead-letter queue
    #
    # The redrive policy is read once per queue instance and cached.
    #
    # @return [Integer, nil] the redrive policy maxReceiveCount, or nil when the queue has no redrive policy
    def max_receive_count
      return @_max_receive_count if defined?(@_max_receive_count)

      policy = queue_attributes.attributes[REDRIVE_POLICY_ATTR]

      @_max_receive_count = policy && JSON.parse(policy)['maxReceiveCount']&.to_i
    end

    private

    # Logs every per-entry failure from a delete_message_batch response. Extracted so both
    # the boolean {#delete_messages} contract and the settlement/retry path log identically.
    #
    # @param failed_messages [Array<Object>] AWS BatchResultErrorEntry objects or compatible hashes
    # @return [void]
    def log_delete_failures(failed_messages)
      # Log every failure (not just the first). The previous `any? { logger.error ... }`
      # short-circuited after the first failure - so the rest went unlogged - and only
      # returned truthy because Logger#error happens to return true.
      Array(failed_messages).each do |failure|
        logger.error do
          "Could not delete #{failure_id(failure)}, code: '#{failure_code(failure)}', " \
            "message: '#{failure_message(failure)}', sender_fault: #{failure_sender_fault(failure)}"
        end
      end
    end

    # Reads the +failed+ entries from an AWS result type or a Hash response.
    #
    # @param response [Object, Hash] the batch response
    # @return [Array<Object>] the failed entries
    def response_failures(response)
      failed = if response.is_a?(Hash)
                 response[:failed] || response['failed']
               elsif response.respond_to?(:failed)
                 response.failed
               end

      Array(failed)
    end

    # @param failure [Object, Hash] the failure entry
    # @return [String, nil]
    def failure_id(failure)
      failure_field(failure, :id)
    end

    # @param failure [Object, Hash] the failure entry
    # @return [String, nil]
    def failure_code(failure)
      failure_field(failure, :code)
    end

    # @param failure [Object, Hash] the failure entry
    # @return [String, nil]
    def failure_message(failure)
      failure_field(failure, :message)
    end

    # @param failure [Object, Hash] the failure entry
    # @return [Boolean, nil]
    def failure_sender_fault(failure)
      failure_field(failure, :sender_fault)
    end

    # Reads a field from an AWS struct or a Hash failure entry.
    #
    # @param failure [Object, Hash] the failure entry
    # @param field [Symbol] the field name
    # @return [Object, nil]
    def failure_field(failure, field)
      if failure.is_a?(Hash)
        failure[field] || failure[field.to_s]
      elsif failure.respond_to?(field)
        failure.public_send(field)
      end
    end

    # Logs any per-entry failures from a batch send. SQS reports these in response.failed (throttling, oversize/malformed
    # entries, or a same-batch duplicate message_deduplication_id) rather than raising, so without this they are silently
    # swallowed - notably by ShoryukenAdapter#enqueue_all, which reads only response.successful. Mirrors delete_messages.
    #
    # @param response [Aws::SQS::Types::SendMessageBatchResult] the batch result
    # @return [void]
    def log_failed_sends(response)
      return unless response.respond_to?(:failed)

      Array(response.failed).each do |failure|
        logger.error do
          "Could not send #{failure.id}, code: '#{failure.code}', message: '#{failure.message}', " \
            "sender_fault: #{failure.sender_fault}"
        end
      end
    end

    # Initializes the FIFO attribute by calling fifo?
    #
    # @return [Boolean] whether the queue is FIFO
    def initialize_fifo_attribute
      # calling fifo? will also initialize it
      fifo?
    end

    # Sets the queue name and URL from a queue name
    #
    # @param name [String] the queue name
    # @return [void]
    def set_by_name(name) # rubocop:disable Naming/AccessorMethodName
      self.name = name
      self.url  = client.get_queue_url(queue_name: name).queue_url
    end

    # Sets the queue name and URL from a queue URL
    #
    # @param url [String] the queue URL
    # @return [void]
    def set_by_url(url) # rubocop:disable Naming/AccessorMethodName
      self.name = url.split('/').last
      self.url  = url
    end

    # Converts an ARN to a queue URL
    #
    # @param arn_str [String] the ARN string
    # @return [String] the queue URL
    def arn_to_url(arn_str)
      *, region, account_id, resource = arn_str.split(':')

      required = [region, account_id, resource].map(&:to_s)
      valid = required.none?(&:empty?)

      unless valid
        raise Errors::InvalidArnError, "Invalid ARN: #{arn_str}. A valid ARN must include: region, account_id and resource."
      end

      "https://sqs.#{region}.amazonaws.com/#{account_id}/#{resource}"
    end

    # Sets the queue name and URL from a name, URL, or ARN
    #
    # @param name_or_url_or_arn [String] the queue identifier
    # @return [void]
    def set_name_and_url(name_or_url_or_arn) # rubocop:disable Naming/AccessorMethodName
      if name_or_url_or_arn.include?('://')
        set_by_url(name_or_url_or_arn)

        # anticipate the fifo? checker for validating the queue URL
        initialize_fifo_attribute
        return
      end

      if name_or_url_or_arn.start_with?('arn:')
        url = arn_to_url(name_or_url_or_arn)
        set_by_url(url)

        # anticipate the fifo? checker for validating the queue URL
        initialize_fifo_attribute
        return
      end

      set_by_name(name_or_url_or_arn)
    rescue Aws::Errors::NoSuchEndpointError, Aws::SQS::Errors::NonExistentQueue
      raise Errors::QueueNotFoundError, "The specified queue #{name_or_url_or_arn} does not exist."
    end

    # Returns the queue attributes from SQS
    #
    # @return [Aws::SQS::Types::GetQueueAttributesResult] the queue attributes
    def queue_attributes
      # Note: Retrieving all queue attributes as requesting `FifoQueue` on non-FIFO queue raises error.
      # See issue: https://github.com/aws/aws-sdk-ruby/issues/1350
      client.get_queue_attributes(queue_url: url, attribute_names: ['All'])
    end

    # Sanitizes a batch of messages, converting to proper format
    #
    # @param options [Hash, Array] batch options or array of messages
    # @option options [Array<Hash>] :entries message entries
    # @return [Hash] the sanitized options with entries key
    def sanitize_messages!(options)
      if options.is_a?(Array)
        entries = options.map.with_index do |m, index|
          { id: index.to_s }.merge(m.is_a?(Hash) ? m : { message_body: m })
        end

        options = { entries: entries }
      end

      options[:entries].each(&method(:sanitize_message!))

      options
    end

    # Adds FIFO attributes to message options if needed
    #
    # @param options [Hash] the message options
    # @option options [String] :message_body the message body
    # @return [Hash] the options with FIFO attributes added
    def add_fifo_attributes!(options)
      return unless fifo?

      options[:message_group_id] ||= MESSAGE_GROUP_ID

      # Auto-generate a content-based dedup id unless disabled. With it on, two
      # sends of an identical body within SQS's 5-minute window are deduplicated
      # (the second is silently dropped); disable it to send such messages
      # distinctly (requires an explicit message_deduplication_id or the queue's
      # ContentBasedDeduplication attribute).
      if Shoryuken.fifo_message_deduplication?
        options[:message_deduplication_id] ||= Digest::SHA256.hexdigest(options[:message_body].to_s)
      end

      options
    end

    # Sanitizes a single message, converting body to JSON if needed
    #
    # @param options [Hash, String] message options or body string
    # @option options [String, Hash] :message_body the message body
    # @return [Hash] the sanitized message options
    def sanitize_message!(options)
      options = { message_body: options } if options.is_a?(String)

      if (body = options[:message_body]).is_a?(Hash)
        options[:message_body] = JSON.dump(body)
      end

      add_fifo_attributes!(options)

      options
    end
  end
end
