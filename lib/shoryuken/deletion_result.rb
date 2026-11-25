# frozen_string_literal: true

module Shoryuken
  # Groups the per-receipt outcome of SQS delete_message_batch call(s).
  #
  # A batch HTTP response can be successful while individual receipts are listed in
  # +failed+. This settlement keeps the requested entries together with the AWS
  # per-entry results, classifies each failure as retryable or terminal, and - when
  # retries are driven through {Shoryuken::Queue#delete_messages_settlement} - folds
  # every attempt into one final view where each receipt ends up deleted, still
  # transiently failed, or terminally rejected (for example +ReceiptHandleIsInvalid+).
  #
  # The object is intentionally easy to inspect from a probe or console:
  #
  #   settlement.successful          # entries confirmed by SQS
  #   settlement.retryable_failures  # transient AWS failure entries
  #   settlement.terminal_failures   # expired/invalid receipts and other sender faults
  #   settlement.retry_entries       # request entries to send on the next batch call
  #   settlement.receipts            # every requested receipt with its final state
  #   settlement.to_h                # JSON-serializable grouped snapshot
  class DeletionResult
    # Per-receipt final state stored in {#receipts}.
    Receipt = Struct.new(:id, :receipt_handle, :status, :code, :message, :sender_fault, :attempts,
                         keyword_init: true) do
      # @return [Boolean] whether SQS confirmed the deletion
      def deleted?
        status == :deleted
      end

      # @return [Boolean] whether the failure is transient and safe to retry
      def retryable?
        status == :retryable_failed
      end

      # @return [Boolean] whether the receipt was permanently rejected (do not retry)
      def terminal?
        status == :terminal_failed
      end

      # @return [Boolean] whether this receipt was sent in more than one batch attempt
      def retried?
        attempts.to_i > 1
      end

      # AWS BatchResultErrorEntry uses +sender_fault+; expose the same predicate style.
      #
      # @return [Boolean] whether SQS attributed the failure to the request
      def sender_fault?
        !!sender_fault
      end
    end

    # Failure codes that unambiguously identify a transient broker-side condition for
    # which re-issuing the very same delete request (same receipt handle) is sensible.
    # Matching is code based rather than on +sender_fault+ alone so unknown sender-side
    # codes (malformed ids, expired handles, ...) stay terminal unless they clearly name
    # a temporary condition.
    RETRYABLE_CODE_PATTERN =
      /internalerror|internalfailure|serviceunavailable|throttl|requesttimeout|timeout|busy|temporar|transient|try.?again/i

    # Codes that look retryable by wording but explicitly ask callers not to retry.
    NON_RETRYABLE_CODE_PATTERN = /non.?retry|do.?not.?retry|permanent/i

    # Known permanent/sender-side codes that must never be retried, even if a stub omits or
    # mislabels +sender_fault+. An expired/invalid receipt handle is the important one.
    TERMINAL_CODE_PATTERN =
      /receipthandleisinvalid|invalididformat|invalidbatchentryid|batchentryidsnotdistinct|toomanyentriesinbatchrequest|emptybatchrequest|accessdenied|notauthorized|operationnotpermitted/i

    # @return [Array<Hash>] the request entries this settlement started from
    attr_reader :entries

    # @return [Array<Object>] every AWS response recorded into this settlement
    attr_reader :responses

    # Creates a settlement for the request entries about to be deleted.
    #
    # @param entries [Array<Hash>] the +{id:, receipt_handle:}+ request entries
    def initialize(entries = [])
      @entries = Array(entries)
      @responses = []
      @receipts_by_id = {}

      @entries.each do |entry|
        id = entry_value(entry, :id)
        @receipts_by_id[id] = Receipt.new(
          id: id,
          receipt_handle: entry_value(entry, :receipt_handle),
          status: :pending,
          attempts: 0
        )
      end
    end

    # Builds a settlement and records one AWS batch response in a single step.
    #
    # @param entries [Array<Hash>] the request entries that were sent
    # @param response [Object, Hash, nil] a delete_message_batch response or response-like object
    # @return [DeletionResult] the recorded settlement
    def self.from_response(entries, response)
      new(entries).record(response)
    end

    # Whether an object carries per-entry batch outcome data that {#record} understands.
    #
    # @param response [Object] the candidate object
    # @return [Boolean] true for AWS result types, compatible doubles and Hashes
    def self.recordable?(response)
      response.respond_to?(:failed) || response.is_a?(Hash)
    end

    # Absorbs one AWS +delete_message_batch+ response, updating per-receipt state.
    #
    # Successful ids become +:deleted+ (a deleted receipt stays deleted even if a later
    # retried batch omits it); failed ids become +:retryable_failed+ or
    # +:terminal_failed+. Failure entries whose id was not among the request entries are
    # still exposed through {#failed} and the grouping methods.
    #
    # @param response [Aws::SQS::Types::DeleteMessageBatchResult, Hash, nil] the batch response
    # @return [self]
    def record(response)
      return self unless self.class.recordable?(response)

      @responses << response

      each_entry(read_batch_field(response, :successful)) do |item|
        id = entry_value(item, :id)
        receipt = receipt_for(id)
        receipt.status = :deleted
        receipt.attempts += 1
      end

      each_entry(read_batch_field(response, :failed)) do |failure|
        id = entry_value(failure, :id)
        code = entry_value(failure, :code)
        sender_fault = entry_value(failure, :sender_fault)
        retryable = retryable_failure?(code, sender_fault)

        receipt = receipt_for(id)
        receipt.attempts += 1
        receipt.code = code
        receipt.message = entry_value(failure, :message)
        receipt.sender_fault = sender_fault
        receipt.status = retryable ? :retryable_failed : :terminal_failed
      end

      self
    end

    # Classifies an SQS failure code.
    #
    # @param code [String, Symbol, nil] the AWS failure code
    # @return [Boolean] true only for codes that clearly name a transient condition
    def retryable_code?(code)
      code = code.to_s
      return false if code.empty?
      return false if code.match?(NON_RETRYABLE_CODE_PATTERN)

      code.match?(RETRYABLE_CODE_PATTERN)
    end

    # Classifies a whole per-entry failure using its code and the AWS +sender_fault+ flag.
    #
    # A failure is retried only when it is clearly transient: a known temporary code, or a
    # non-sender fault (+sender_fault: false+, i.e. an AWS-side condition). Known permanent
    # sender codes (invalid/expired receipt handles, malformed ids, access denied, ...) are
    # never retried even if the stub omits the flag. Unknown codes without an explicit
    # +sender_fault: false+ stay terminal - only clearly retryable deletes are redone.
    #
    # @param code [String, Symbol, nil] the AWS failure code
    # @param sender_fault [Boolean, nil] the AWS sender_fault flag
    # @return [Boolean] whether the same delete request should be retried
    def retryable_failure?(code, sender_fault = nil)
      code_str = code.to_s

      return false if code_str.match?(TERMINAL_CODE_PATTERN)
      return false if code_str.match?(NON_RETRYABLE_CODE_PATTERN)

      retryable_code?(code) || sender_fault == false
    end

    # @return [Array<Receipt>] every requested receipt with its final state
    def receipts
      @receipts_by_id.values
    end

    # Request entries confirmed deleted by SQS.
    #
    # @return [Array<Receipt>] deleted receipts
    def successful
      receipts.select(&:deleted?)
    end
    alias successes successful
    alias deleted successful

    # All receipts that ended in a failure state, including retryable failures that still
    # failed after the last attempt and terminal failures such as invalid receipts.
    #
    # @return [Array<Receipt>] failed receipts (quack like AWS BatchResultErrorEntry)
    def failed
      receipts.select { |receipt| %i[retryable_failed terminal_failed].include?(receipt.status) }
    end

    # @return [Array<Receipt>] transient failures that may be retried
    def retryable_failures
      receipts.select(&:retryable?)
    end
    alias retryable retryable_failures
    alias retriable_failures retryable_failures
    alias retriable retryable_failures
    alias transient_failures retryable_failures

    # @return [Array<Receipt>] permanent failures that must not be retried
    def terminal_failures
      receipts.select(&:terminal?)
    end
    alias permanent_failures terminal_failures
    alias invalid_failures terminal_failures

    # @return [Array<Receipt>] receipts that were re-sent in a targeted retry batch
    def retried_receipts
      receipts.select(&:retried?)
    end

    # Number of delete_message_batch API calls folded into this settlement.
    #
    # @return [Integer]
    def api_calls
      @responses.size
    end
    alias attempts api_calls
    alias retry_count api_calls

    # Groups every receipt by its final status - the "grouped reconciliation" view.
    #
    # @return [Hash{Symbol => Array<Receipt>}]
    def group_by_status
      receipts.group_by(&:status)
    end
    alias groups group_by_status

    # Builds the +:entries+ argument for the next targeted batch delete: only the
    # receipts whose latest failure is clearly transient, preserving the original
    # +id+/+receipt_handle+ pair so AWS correlates the result.
    #
    # @return [Array<Hash>] request entries for the retry call
    def retry_entries
      retryable_failures.map do |receipt|
        { id: receipt.id, receipt_handle: receipt.receipt_handle }
      end
    end
    alias retryable_entries retry_entries
    alias retriable_entries retry_entries

    # @return [Array<Hash>] request entries for receipts that were re-sent at least once
    def retried_entries
      retried_receipts.map { |receipt| { id: receipt.id, receipt_handle: receipt.receipt_handle } }
    end

    # @return [Boolean] true when every requested receipt was confirmed deleted
    def success?
      receipts.any? && receipts.all?(&:deleted?)
    end
    alias successful? success?
    alias complete? success?

    # Mirrors the historical +Queue#delete_messages+ boolean contract: true when at
    # least one receipt is still in a failure state after all recorded attempts.
    #
    # @return [Boolean] whether any failure remains
    def failed?
      failed.any?
    end

    # @return [Boolean] whether a further targeted retry makes sense
    def retryable?
      retryable_failures.any?
    end

    # Grouped, JSON-serializable snapshot; this is what a probe records as
    # +middleware_result+ / final receipts.
    #
    # @return [Hash]
    def to_h
      {
        success: success?,
        api_calls: api_calls,
        successful: successful.map(&method(:receipt_hash)),
        failed: failed.map(&method(:receipt_hash)),
        retryable_failures: retryable_failures.map(&method(:receipt_hash)),
        terminal_failures: terminal_failures.map(&method(:receipt_hash)),
        retried_entries: retried_entries,
        retry_entries: retry_entries,
        receipts: receipts.map(&method(:receipt_hash))
      }
    end

    # ActiveSupport/JSON convention; the optional options argument is accepted and ignored.
    #
    # @param _options [Object, nil] encoder options
    # @return [Hash]
    def as_json(_options = nil)
      to_h
    end

    # @param _args [Object] generator state (ignored; a fresh document is generated)
    # @return [String] JSON representation of {#to_h}
    def to_json(*_args)
      JSON.generate(to_h)
    end

    private

    # Finds or creates the receipt line for an AWS entry id. Failure ids that were not
    # part of the original request still get an inspectable line.
    #
    # @param id [String] the AWS batch entry id
    # @return [Receipt]
    def receipt_for(id)
      @receipts_by_id[id] ||= Receipt.new(id: id, status: :terminal_failed, attempts: 0)
    end

    # Reads a field from an AWS type or a Hash response.
    #
    # @param response [Object, Hash] the response
    # @param field [Symbol] the snake_case field name
    # @return [Object, nil]
    def read_batch_field(response, field)
      if response.is_a?(Hash)
        response[field] || response[field.to_s]
      elsif response.respond_to?(field)
        response.public_send(field)
      end
    end

    # Reads a value from an AWS struct or a Hash entry, trying symbol then string keys.
    #
    # @param entry [Object, Hash] the entry
    # @param field [Symbol] the field name
    # @return [Object, nil]
    def entry_value(entry, field)
      if entry.is_a?(Hash)
        entry[field] || entry[field.to_s]
      elsif entry.respond_to?(field)
        entry.public_send(field)
      end
    end

    # AWS batch fields are enumerables of structs; tolerate nil and single values.
    #
    # @param value [Object] the field value
    # @yieldparam item [Object] each entry
    def each_entry(value)
      Array(value).each { |item| yield(item) if item }
    end

    # @param receipt [Receipt] the receipt
    # @return [Hash] primitive representation
    def receipt_hash(receipt)
      {
        id: receipt.id,
        receipt_handle: receipt.receipt_handle,
        status: receipt.status,
        code: receipt.code,
        message: receipt.message,
        sender_fault: receipt.sender_fault,
        attempts: receipt.attempts
      }.compact
    end
  end
end
