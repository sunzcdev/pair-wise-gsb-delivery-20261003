# encoding: utf-8
# frozen_string_literal: true

module Mail
  class DeliveryStatus
    class ParseError < StandardError; end

    class FieldGroup
      def initialize(fields)
        @fields = fields
      end

      attr_reader :fields

      def [](name)
        fields = @fields.select { |field| field.name.casecmp?(name) }
        return nil if fields.empty?

        values = fields.map(&:value)
        values.size == 1 ? values.first : values
      end

      def fetch(name)
        self[name] or
          raise ParseError, "delivery status record is missing required field #{name.inspect}"
      end

      def action
        self['action']
      end

      def status
        self['status']
      end

      def bounced?
        !!(action =~ /failed/i)
      end

      def retryable?
        !!(status !~ /\A5\./)
      end

      def to_h
        @fields.each_with_object({}) do |field, fields|
          name = field.name.downcase

          if fields.key?(name)
            fields[name] = Array(fields[name]) << field.value
          else
            fields[name] = field.value
          end
        end
      end

      def encoded
        @fields.map(&:encoded).join
      end
    end

    MESSAGE_REQUIRED_FIELDS = %w[reporting-mta].freeze
    RECIPIENT_REQUIRED_FIELDS = %w[final-recipient action status].freeze
    private_constant :MESSAGE_REQUIRED_FIELDS, :RECIPIENT_REQUIRED_FIELDS

    def self.parse(source)
      normalized_source = normalize_source(source)
      validate_continuations(normalized_source)
      blocks = split_blocks(normalized_source)
      groups = []
      position = 0

      blocks.each do |block|
        start_index = normalized_source.index(block, position)
        line_offset = normalized_source[0...start_index].count("\r\n")
        groups << parse_block(block, line_offset)
        position = start_index + block.length
      end

      new(groups)
    end

    def self.normalize_source(source)
      source = Utilities.to_crlf(source.to_s).gsub(/\A[\r\n]+|[\r\n]+\z/, '')
      raise ParseError, 'delivery status report is empty' if source.empty?

      source
    end
    private_class_method :normalize_source

    def self.validate_continuations(source)
      previous_line_was_field = false

      source.lines.each_with_index do |line, index|
        if line =~ /\A[ \t]*\r\n\z/
          previous_line_was_field = false
        elsif line =~ /\A[ \t]/
          unless previous_line_was_field
            raise ParseError, "delivery status field continuation on line #{index + 1} does not follow a field"
          end
        elsif line =~ /\A[^\r\n:]+:/
          previous_line_was_field = true
        else
          raise ParseError, "malformed delivery status field on line #{index + 1}: #{line.inspect}"
        end
      end
    end
    private_class_method :validate_continuations

    def self.split_blocks(source)
      blocks = source.split(/\r\n(?=[ \t]*\r\n)(?:[ \t]*\r\n)*/)
      raise ParseError, 'delivery status report must contain a message field group' if blocks.empty?

      blocks
    end
    private_class_method :split_blocks

    def self.parse_block(block, line_offset = 0)
      fields = []
      unfolded_block = block.gsub(/\r\n([ \t])/, '\1')

      unfolded_block.each_line("\r\n").with_index do |line, index|
        name, value = parse_line(line.chomp("\r\n"), line_offset + index + 1)
        fields << Mail::Field.new(name, value)
      end

      FieldGroup.new(fields)
    end
    private_class_method :parse_block

    def self.parse_line(line, line_number)
      name, separator, value = line.partition(':')

      unless separator == ':' && name =~ /\A[!-9;-\~]+\z/
        raise ParseError, "malformed delivery status field on line #{line_number}: #{line.inspect}"
      end

      name = name.rstrip
      value = value.to_s.strip

      [name, value]
    end
    private_class_method :parse_line

    def initialize(groups)
      raise ParseError, 'delivery status report must contain a message field group' if groups.empty?

      @message_fields = groups.first
      @recipient_fields = groups[1..]

      validate_group(@message_fields, MESSAGE_REQUIRED_FIELDS, 'message')
      if @recipient_fields.empty?
        raise ParseError, 'delivery status report must contain at least one recipient field group'
      end

      @recipient_fields.each_with_index do |group, index|
        validate_group(group, RECIPIENT_REQUIRED_FIELDS, "recipient #{index + 1}")
      end
    end

    attr_reader :message_fields, :recipient_fields

    alias_method :recipients, :recipient_fields
    alias_method :recipient_records, :recipient_fields

    def action
      recipient_values('action')
    end

    def status
      recipient_values('status')
    end

    alias_method :error_status, :status

    def final_recipient
      recipient_values('final-recipient')
    end

    def diagnostic_code
      recipient_values('diagnostic-code')
    end

    def remote_mta
      recipient_values('remote-mta')
    end

    def reporting_mta
      message_fields['reporting-mta']
    end

    def [](name)
      recipient_values(name)
    end

    def bounced?
      recipient_fields.first&.bounced?
    end

    def retryable?
      recipient_fields.first&.retryable?
    end

    def encoded
      ([message_fields] + recipient_fields).map(&:encoded).join("\r\n")
    end

    def to_s
      encoded
    end

    private

    def recipient_values(name)
      return nil if recipient_fields.empty?

      values = recipient_fields.map { |group| group[name] }
      values.size == 1 ? values.first : values
    end

    def validate_group(group, required_fields, label)
      required_fields.each do |name|
        value = group[name]

        if value.nil? || Array(value).any?(&:nil?) || value.to_s.empty?
          raise ParseError, "#{label} delivery status record is missing required field #{name.inspect}"
        end
      end
    end
  end
end
