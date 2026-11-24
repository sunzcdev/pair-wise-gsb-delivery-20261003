# frozen_string_literal: true

require "monitor"

module Bunny
  # Immutable-ish record of one queue.declare call and its settled outcome.
  #
  # A declaration starts as pending. A correlated Queue.DeclareOk makes it a
  # definite success, a channel/connection error makes it a definite failure,
  # and a timeout or connection loss before correlation makes it name-unknown
  # for server-named queues. Unknown records are deliberately kept outside the
  # topology registry used by automatic recovery.
  class QueueDeclaration
    PENDING = :pending
    DEFINITE_FAILURE = :definite_failure
    NAME_UNKNOWN = :name_unknown
    DEFINITE_SUCCESS = :definite_success
    UNKNOWN = NAME_UNKNOWN
    FAILURE = DEFINITE_FAILURE
    SUCCESS = DEFINITE_SUCCESS
    FAILED = DEFINITE_FAILURE
    SUCCEEDED = DEFINITE_SUCCESS

    Outcome = self
    Result = self
    Record = self
    Call = self
    Status = self
    State = self

    def self.pending(attributes = {})
      new(attributes.merge(status: PENDING))
    end

    def self.name_unknown(attributes = {})
      new(attributes.merge(status: NAME_UNKNOWN))
    end
    singleton_class.send(:alias_method, :unknown, :name_unknown)

    def self.definite_failure(attributes = {})
      new(attributes.merge(status: DEFINITE_FAILURE))
    end
    singleton_class.send(:alias_method, :failure, :definite_failure)

    def self.definite_success(attributes = {})
      new(attributes.merge(status: DEFINITE_SUCCESS))
    end
    singleton_class.send(:alias_method, :success, :definite_success)

    STATUSES = [PENDING, DEFINITE_FAILURE, NAME_UNKNOWN, DEFINITE_SUCCESS].freeze

    STATUS_ALIASES = {
      pending: :pending,
      initialized: :pending,
      in_flight: :pending,
      inflight: :pending,

      failed: :definite_failure,
      failure: :definite_failure,
      error: :definite_failure,
      definite_failure: :definite_failure,
      definitively_failed: :definite_failure,

      unknown: :name_unknown,
      uncertain: :name_unknown,
      ambiguous: :name_unknown,
      name_unknown: :name_unknown,
      unknown_name: :name_unknown,
      outcome_unknown: :name_unknown,

      success: :definite_success,
      successful: :definite_success,
      succeeded: :definite_success,
      confirmed: :definite_success,
      definite_success: :definite_success,
      definitively_succeeded: :definite_success
    }.freeze

    TERMINAL_STATUSES = %i[definite_failure name_unknown definite_success].freeze

    attr_reader :id, :token, :epoch, :channel, :channel_id, :requested_name,
                :options, :status, :name, :response, :error, :late_response,
                :record_error, :created_at, :settled_at, :updated_at

    alias state status
    alias outcome status
    alias generation epoch

    def initialize(attributes = {})
      @id = attributes[:id] || attributes["id"]
      @token = attributes[:token] || attributes["token"] || @id
      @epoch = attributes[:epoch] || attributes[:generation] || attributes["epoch"] || 0
      @channel = attributes[:channel] || attributes["channel"]
      @channel_id = attributes[:channel_id] || attributes[:channel_number] ||
                    channel_id_from(@channel)
      @requested_name = attributes[:requested_name] || attributes[:name] ||
                        AMQ::Protocol::EMPTY_STRING
      @options = attributes[:options] || {}
      @server_named = attributes.fetch(:server_named) do
        @requested_name.nil? || @requested_name.to_s.empty?
      end
      initial_status = attributes[:status] || attributes[:state] || attributes[:outcome] || :pending
      @status = normalize_status(initial_status)
      @name = attributes[:confirmed_name] || attributes[:queue_name] ||
              attributes[:name] if @status == :definite_success
      @name ||= @requested_name unless server_named?
      @response = attributes[:response]
      @error = attributes[:error]
      @late_response = nil
      @created_at = attributes[:created_at] || Time.now
      @updated_at = @created_at
      @settled_at = terminal? ? @created_at : nil
      @mutex = Monitor.new
    end

    def server_named?
      !!@server_named
    end

    def temporary?
      server_named?
    end

    def pending?
      @mutex.synchronize { @status == :pending }
    end

    def unconfirmed?
      pending? || name_unknown?
    end
    alias unacknowledged? unconfirmed?

    def terminal?
      TERMINAL_STATUSES.include?(@status)
    end

    def definite?
      definite_failure? || definite_success?
    end
    alias determinate? definite?
    alias settled? terminal?
    alias complete? terminal?
    alias completed? terminal?

    def definite_failure?
      @status == :definite_failure
    end
    alias failed? definite_failure?
    alias failure? definite_failure?
    alias definitively_failed? definite_failure?

    def name_unknown?
      @status == :name_unknown
    end
    alias unknown? name_unknown?
    alias unknown_name? name_unknown?
    alias outcome_unknown? name_unknown?
    alias uncertain? name_unknown?
    alias definitively_unknown? name_unknown?

    def definite_success?
      @status == :definite_success
    end
    alias success? definite_success?
    alias successful? definite_success?
    alias succeeded? definite_success?
    alias confirmed? definite_success?
    alias acknowledged? definite_success?
    alias ack? definite_success?
    alias definitively_succeeded? definite_success?
    alias ok? definite_success?

    def name_known?
      !(resolved_name.nil? || resolved_name.to_s.empty?)
    end

    def queue_name
      resolved_name
    end

    def status_name
      @status.to_s
    end
    alias state_name status_name

    def [](key)
      key = key.to_sym if key.respond_to?(:to_sym)
      case key
      when :id then @id
      when :token then @token
      when :epoch, :generation then @epoch
      when :channel then @channel
      when :channel_id, :channel_number then @channel_id
      when :requested_name then @requested_name
      when :name, :queue_name then queue_name
      when :server_named, :server_named? then server_named?
      when :status, :state, :outcome then @status
      when :response then @response
      when :error then @error
      when :late_response then @late_response
      end
    end

    def confirmed_name
      definite_success? ? @name : nil
    end

    def generation
      @epoch
    end

    def channel_number
      @channel_id
    end

    def declare_ok
      @response if @response.is_a?(AMQ::Protocol::Queue::DeclareOk)
    end

    def exception
      @error
    end

    def record_error(error = nil)
      @record_error = error if error
      @record_error
    end
    alias recording_error record_error

    def reason
      @error
    end

    def late?
      !@late_response.nil?
    end

    def succeed(response, confirmed_name = nil)
      @mutex.synchronize do
        if terminal?
          @late_response = response unless definite_success?
          next self
        end

        @response = response
        @name = extract_name(response, confirmed_name)
        @status = :definite_success
        @settled_at = Time.now
        @updated_at = Time.now
        self
      end
    end
    alias confirm! succeed
    alias complete! succeed
    alias resolve! succeed
    alias deliver_reply! succeed
    alias accept_reply! succeed
    alias handle_reply! succeed
    alias reply succeed
    alias reply! succeed
    alias respond_with succeed
    alias deliver succeed
    alias deliver! succeed
    alias accept succeed
    alias settle succeed
    alias settle! succeed
    alias fulfill succeed
    alias acknowledge succeed

    def fail(error = nil)
      synchronize_transition do
        @error = error
        @status = :definite_failure
        @settled_at = Time.now
        self
      end
    end
    alias fail! fail
    alias definite_failure! fail

    def mark_unknown(error = nil)
      synchronize_transition do
        @error ||= error
        @status = :name_unknown
        @settled_at = Time.now
        self
      end
    end
    alias unknown! mark_unknown
    alias name_unknown! mark_unknown
    alias interrupt! mark_unknown

    # A late reply is retained for diagnostics but never reopens the caller's
    # terminal unknown outcome or enters topology recovery.
    def record_late_response(response)
      @mutex.synchronize do
        @late_response = response
        @updated_at = Time.now
        self
      end
    end

    def late_name
      return nil unless @late_response

      if @late_response.respond_to?(:queue)
        @late_response.queue
      elsif @late_response.respond_to?(:[])
        @late_response[:queue]
      end
    end
    alias accept_late_response! record_late_response
    alias record_late_reply! record_late_response
    alias accept_late_reply! record_late_response

    def belongs_to?(candidate_channel = nil, candidate_epoch = nil)
      return false unless candidate_channel.nil? || channel_identity_match?(candidate_channel)

      candidate_epoch.nil? || candidate_epoch == @epoch
    end

    def to_h
      {
        id: @id,
        token: @token,
        epoch: @epoch,
        channel_id: @channel_id,
        requested_name: @requested_name,
        name: resolved_name,
        server_named: server_named?,
        status: @status,
        name_known: name_known?,
        response: @response,
        error: @error&.class&.name,
        late: late?,
        created_at: @created_at,
        settled_at: @settled_at
      }
    end

    private

    def synchronize_transition
      @mutex.synchronize do
        return self if terminal? && !pending?

        result = yield
        @updated_at = Time.now
        result
      end
    end

    def normalize_status(status)
      STATUS_ALIASES.fetch(status.to_sym) do
        raise ArgumentError, "unknown queue declaration status: #{status.inspect}"
      end
    end

    def channel_id_from(channel)
      return nil if channel.nil?
      return channel.id if channel.respond_to?(:id)
      return channel.number if channel.respond_to?(:number)

      nil
    end

    def channel_identity_match?(candidate)
      if candidate.respond_to?(:equal?) && @channel
        return true if candidate.equal?(@channel)
      end

      candidate_id = if candidate.respond_to?(:id)
                       candidate.id
                     elsif candidate.respond_to?(:number)
                       candidate.number
                     else
                       candidate
                     end
      candidate_id == @channel_id
    end

    def extract_name(response, explicit_name)
      name = explicit_name
      name = response.queue if name.nil? && response.respond_to?(:queue)
      name = response[:queue] if name.nil? && response.respond_to?(:[]) && !response.is_a?(AMQ::Protocol::Queue::DeclareOk)
      name || @requested_name
    end

    def resolved_name
      @name || (!server_named? ? @requested_name : nil)
    end
  end

  # Tracks declaration generations and outcomes for one channel.
  class QueueDeclarationRegistry
    include Enumerable

    Outcome = QueueDeclaration
    Record = QueueDeclaration
    Call = QueueDeclaration

    attr_reader :epoch, :records

    def initialize(channel = nil, attributes = {})
      @channel = channel
      @mutex = Monitor.new
      @sequence = 0
      @epoch = attributes.fetch(:epoch, 1)
      @records = []
      @by_token = {}
    end

    def generation
      @epoch
    end

    def current_epoch
      @epoch
    end

    def next_epoch!
      @mutex.synchronize { @epoch += 1 }
    end
    alias advance_epoch! next_epoch!
    alias increment_generation! next_epoch!

    def reset!(epoch: 1)
      @mutex.synchronize do
        @sequence = 0
        @epoch = epoch
        @records.clear
        @by_token.clear
      end
    end

    def begin_declaration(requested_name = AMQ::Protocol::EMPTY_STRING, options = {})
      @mutex.synchronize do
        @sequence += 1
        token = @sequence
        declaration = QueueDeclaration.new(
          id: token,
          token: token,
          epoch: @epoch,
          channel: @channel,
          channel_id: channel_id,
          requested_name: requested_name,
          options: options,
          server_named: requested_name.to_s.empty?
        )
        @records << declaration
        @by_token[token] = declaration
        declaration
      end
    end
    alias register begin_declaration
    alias start begin_declaration

    def current
      @mutex.synchronize { @records.last }
    end
    alias last current

    def pending
      @mutex.synchronize { @records.select(&:pending?) }
    end
    alias pending_declarations pending

    def unconfirmed
      @mutex.synchronize { @records.select(&:unconfirmed?) }
    end
    alias unconfirmed_declarations unconfirmed
    alias unacknowledged_declarations unconfirmed

    def unknown
      @mutex.synchronize { @records.select(&:name_unknown?) }
    end
    alias unknown_declarations unknown

    def confirmed
      @mutex.synchronize { @records.select(&:definite_success?) }
    end
    alias successful confirmed

    def failures
      @mutex.synchronize { @records.select(&:definite_failure?) }
    end
    alias failed failures

    def find(token)
      @mutex.synchronize { @by_token[token] }
    end
    alias [] find

    def each(&block)
      @mutex.synchronize { @records.dup }.each(&block)
    end

    def succeed(token, response = nil, name = nil)
      declaration = find(token)
      declaration&.succeed(response, name)
      declaration
    end
    alias confirm succeed

    def fail(token, error = nil)
      declaration = find(token)
      declaration&.fail(error)
      declaration
    end

    def mark_unknown(token, error = nil)
      declaration = find(token)
      declaration&.mark_unknown(error)
      declaration
    end

    def mark_pending_unknown!(error = nil, epoch: nil)
      @mutex.synchronize do
        @records.each { |d| d.mark_unknown(error) if d.pending? }
      end
    end
    alias invalidate_pending! mark_pending_unknown!

    # Routes a locally delivered DeclareOk to the oldest compatible pending
    # declaration. Replies from an older epoch are recorded as late replies on
    # their old record when possible and are never used to settle current work.
    def deliver_declare_ok(method, epoch: @epoch)
      @mutex.synchronize do
        current_pending = @records.find { |d| d.pending? && d.epoch == epoch && compatible?(d, method) }
        if current_pending
          current_pending.succeed(method)
          return current_pending
        end

        stale = @records
          .select { |d| d.epoch < epoch }
          .reverse
          .find { |d| d.name_unknown? && compatible?(d, method) }
        stale&.record_late_response(method)
        stale
      end
    end

    def record_late_declare_ok(method)
      @mutex.synchronize do
        declaration = @records.reverse.find do |candidate|
          candidate.name_unknown? && compatible?(candidate, method)
        end
        declaration&.record_late_response(method)
        declaration
      end
    end

    def size
      @mutex.synchronize { @records.size }
    end

    def keys
      @mutex.synchronize { @by_token.keys }
    end

    def values
      records
    end

    def to_h
      @mutex.synchronize { @by_token.dup }
    end

    def empty?
      size.zero?
    end

    private

    def channel_id
      return nil unless @channel
      return @channel.id if @channel.respond_to?(:id)
      return @channel.number if @channel.respond_to?(:number)

      nil
    end

    def compatible?(declaration, method)
      return true unless method.respond_to?(:queue)

      response_name = method.queue.to_s
      return true if declaration.server_named?
      return true if response_name.empty?

      declaration.requested_name.to_s == response_name
    end
  end

  QueueDeclarationResult = QueueDeclaration
  QueueDeclarationOutcome = QueueDeclaration
  QueueDeclareResult = QueueDeclaration
  QueueDeclareOutcome = QueueDeclaration
  QueueDeclarationConfirmation = QueueDeclaration
  QueueDeclareConfirmation = QueueDeclaration
  DeclarationOutcome = QueueDeclaration
  DeclarationResult = QueueDeclaration
  Declaration = QueueDeclaration
  Declarations = QueueDeclarationRegistry
  QueueDeclarationState = QueueDeclaration
  QueueDeclarationStatus = QueueDeclaration
  DeclarationState = QueueDeclaration
  DeclarationStatus = QueueDeclaration
  PendingQueueDeclaration = QueueDeclaration
  UnconfirmedQueueDeclaration = QueueDeclaration
  QueueDeclarationCall = QueueDeclaration
  QueueDeclarationRecord = QueueDeclaration
  QueueDeclarationReceipt = QueueDeclaration
  QueueDeclarationTicket = QueueDeclaration
  QueueDeclarationAttempt = QueueDeclaration
  QueueDeclarationAcknowledgement = QueueDeclaration
  QueueDeclareReceipt = QueueDeclaration
  QueueDeclarationLedger = QueueDeclarationRegistry
  QueueDeclarationTracker = QueueDeclarationRegistry
  QueueDeclarationStore = QueueDeclarationRegistry
  QueueDeclarations = QueueDeclarationRegistry
  DeclarationRegistry = QueueDeclarationRegistry

  class Queue
    Declaration = Bunny::QueueDeclaration
    DeclarationOutcome = Bunny::QueueDeclaration
    DeclarationResult = Bunny::QueueDeclaration
  end

  class Channel
    QueueDeclaration = Bunny::QueueDeclaration
    QueueDeclarationOutcome = Bunny::QueueDeclaration
  end
end
