# encoding: utf-8
# frozen_string_literal: true
module Mail

  # Parses and re-encodes the body of a message/delivery-status part as
  # defined by RFC 3464.
  #
  # A delivery status body is made of two kinds of field groups, separated
  # by blank lines:
  #
  # * one message-level field group (Reporting-MTA, Arrival-Date, ...)
  # * one field group per recipient the report talks about
  #
  # A single bounce message can therefore report a different outcome for
  # each recipient: one delivered, one delayed, one permanently failed.
  #
  #   status = Mail::DeliveryStatus.parse(part.body.decoded)
  #   status.reporting_mta                    # => "dns; mail.example.net"
  #   status.recipient_records.length         # => 3
  #   status.recipient_records.map(&:action)  # => ["delivered", "delayed", "failed"]
  #
  # Parsing is strict: a folded continuation line without a preceding field,
  # a line that is not a valid "Name: value" field, or a recipient record
  # missing a required field (Final-Recipient, Action or Status) raises a
  # Mail::DeliveryStatus::ParseError that names the offending record. Each
  # record is validated on its own; a missing field is never filled in from
  # a neighbouring recipient's record.
  class DeliveryStatus

    # Raised when a message/delivery-status body cannot be parsed.
    class ParseError < Mail::Field::FieldError; end

    # Raised when a recipient record lacks a field required by RFC 3464
    # (Final-Recipient, Action or Status).
    class MissingFieldError < ParseError; end

    CRLF = "\r\n".freeze

    # Standard message-level fields from RFC 3464. Anything else found in
    # the first field group is kept as a message-level extension.
    MESSAGE_FIELD_NAMES = %w[
      reporting-mta dsn-gateway received-from-mta arrival-date
      original-envelope-id
    ].freeze

    # Standard per-recipient fields from RFC 3464 and its registered
    # extensions. Anything else in a recipient record is preserved and
    # exposed through RecipientRecord#extensions.
    RECIPIENT_FIELD_NAMES = %w[
      original-recipient final-recipient action status remote-mta
      diagnostic-code last-attempt-date final-log-id will-retry-until
    ].freeze

    # Fields every recipient record must carry per RFC 3464 section 2.3.
    REQUIRED_RECIPIENT_FIELDS = %w[final-recipient action status].freeze

    # One "Name: value" field, keeping its original (possibly folded) raw
    # lines so the body can be re-encoded byte for byte.
    class FieldEntry
      attr_reader :name, :raw_lines

      def initialize(name, raw_lines)
        @name = name
        @raw_lines = raw_lines
      end

      # The unfolded field value: continuation lines are joined back onto
      # the first line, keeping their whitespace, per RFC 5322 unfolding.
      def value
        first = raw_lines.first.sub(/\A[!-9;-~]+:[ \t]*/, '')
        ([first] + raw_lines[1..-1].to_a).join('').strip
      end

      def to_s
        raw_lines.join(CRLF)
      end
    end

    # The delivery status of a single recipient.
    class RecipientRecord
      # 1-based position of this record in the delivery status body.
      attr_reader :index

      def initialize(entries, index)
        @entries = entries
        @index = index
        validate_required_fields!
      end

      # Value of the named field (case-insensitive), or nil.
      def [](name)
        entry = find_entry(name)
        entry && entry.value
      end

      # All field values of this record as an ordered hash, keyed by the
      # original field names.
      def fields
        @entries.each_with_object({}) { |entry, hash| hash[entry.name] = entry.value }
      end

      # Non-standard fields (X-* and other extensions) carried by this
      # record, as an ordered hash keyed by the original field names.
      def extensions
        @entries.each_with_object({}) do |entry, hash|
          hash[entry.name] = entry.value unless RECIPIENT_FIELD_NAMES.include?(entry.name.downcase)
        end
      end

      def original_recipient
        self['original-recipient']
      end

      def final_recipient
        self['final-recipient']
      end

      def action
        self['action']
      end

      def status
        self['status']
      end

      def remote_mta
        self['remote-mta']
      end

      def diagnostic_code
        self['diagnostic-code']
      end

      def last_attempt_date
        self['last-attempt-date']
      end

      def final_log_id
        self['final-log-id']
      end

      def will_retry_until
        self['will-retry-until']
      end

      def delivered?
        action_is?('delivered')
      end

      def failed?
        action_is?('failed')
      end

      # True when this recipient permanently failed (action: failed).
      def bounced?
        failed?
      end

      def delayed?
        action_is?('delayed')
      end

      def expanded?
        action_is?('expanded')
      end

      def relayed?
        action_is?('relayed')
      end

      # True when this recipient's status is not a permanent (5.x.x)
      # failure, meaning a retry could still succeed.
      def retryable?
        status.nil? || status !~ /\A5/
      end

      def ==(other)
        other.is_a?(RecipientRecord) && other.fields == fields
      end

      def to_s
        @entries.map(&:to_s).join(CRLF)
      end

      private

      def find_entry(name)
        downcased = name.to_s.downcase
        @entries.detect { |entry| entry.name.downcase == downcased }
      end

      def action_is?(expected)
        !action.nil? && action.casecmp(expected).zero?
      end

      def validate_required_fields!
        REQUIRED_RECIPIENT_FIELDS.each do |field_name|
          next unless find_entry(field_name).nil?

          raise MissingFieldError,
                "message/delivery-status recipient record #{index} is missing " \
                "the required field '#{field_name}'"
        end
      end
    end

    attr_reader :recipient_records

    def self.parse(text)
      new.parse(text)
    end

    def initialize
      @message_entries = []
      @recipient_records = []
    end

    def parse(text)
      blocks = split_blocks(text)
      if blocks.empty?
        raise ParseError, 'message/delivery-status body is empty: ' \
                          'expected message-level fields and at least one recipient record'
      end

      @message_entries = parse_fields(blocks.first, 'the message-level fields')
      blocks[1..-1].to_a.each_with_index do |block, i|
        entries = parse_fields(block, "recipient record #{i + 1}")
        @recipient_records << RecipientRecord.new(entries, i + 1)
      end
      self
    end

    # Value of the named message-level field (case-insensitive), or nil.
    def [](name)
      entry = find_message_entry(name)
      entry && entry.value
    end

    # All message-level field values as an ordered hash, keyed by the
    # original field names.
    def message_fields
      @message_entries.each_with_object({}) { |entry, hash| hash[entry.name] = entry.value }
    end

    # Non-standard message-level fields, as an ordered hash keyed by the
    # original field names.
    def message_extensions
      @message_entries.each_with_object({}) do |entry, hash|
        hash[entry.name] = entry.value unless MESSAGE_FIELD_NAMES.include?(entry.name.downcase)
      end
    end

    def reporting_mta
      self['reporting-mta']
    end

    def dsn_gateway
      self['dsn-gateway']
    end

    def received_from_mta
      self['received-from-mta']
    end

    def arrival_date
      self['arrival-date']
    end

    def original_envelope_id
      self['original-envelope-id']
    end

    # Re-encodes the delivery status body: message-level fields first, then
    # one blank-line-separated record per recipient. Fields keep their
    # original order and folding.
    def to_s
      groups = []
      groups << @message_entries.map(&:to_s).join(CRLF) unless @message_entries.empty?
      @recipient_records.each { |record| groups << record.to_s }
      groups.join(CRLF + CRLF)
    end

    private

    def find_message_entry(name)
      downcased = name.to_s.downcase
      @message_entries.detect { |entry| entry.name.downcase == downcased }
    end

    # Splits the body into field groups on blank lines. Handles both CRLF
    # and LF line endings.
    def split_blocks(text)
      blocks = []
      current = []
      text.to_s.split(/\r\n|\n/).each do |line|
        if line =~ /\A[ \t]*\z/
          blocks << current unless current.empty?
          current = []
        else
          current << line
        end
      end
      blocks << current unless current.empty?
      blocks
    end

    def parse_fields(lines, context)
      entries = []
      lines.each do |line|
        if line =~ /\A[ \t]/
          if entries.empty?
            raise ParseError,
                  "message/delivery-status #{context} begins with a folded " \
                  "continuation line that has no field to continue: #{line.inspect}"
          end
          entries.last.raw_lines << line
        elsif line =~ /\A([!-9;-~]+):/
          entries << FieldEntry.new(Regexp.last_match(1), [line])
        else
          raise ParseError,
                "message/delivery-status #{context} contains a line that is " \
                "not a 'Field-Name: value' field: #{line.inspect}"
        end
      end
      entries
    end
  end
end
