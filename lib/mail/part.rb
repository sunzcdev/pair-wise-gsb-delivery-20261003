# encoding: utf-8
# frozen_string_literal: true
require 'mail/constants'
require 'mail/utilities'

module Mail
  class Part < Message
    # Creates a new empty Content-ID field and inserts it in the correct order
    # into the Header.  The ContentIdField object will automatically generate
    # a unique content ID if you try and encode it or output it to_s without
    # specifying a content id.
    #
    # It will preserve the content ID you specify if you do.
    def add_content_id(content_id_val = '')
      header['content-id'] = content_id_val
    end

    # Returns true if the part has a content ID field, the field may or may
    # not have a value, but the field exists or not.
    def has_content_id?
      header.has_content_id?
    end

    def cid
      add_content_id unless has_content_id?
      Utilities.uri_escape(Utilities.unbracket(content_id))
    end

    def url
      "cid:#{cid}"
    end

    def inline?
      header[:content_disposition].disposition_type == 'inline' if header[:content_disposition].respond_to?(:disposition_type)
    end

    def add_required_fields
      super
      add_content_id if !has_content_id? && inline?
    end

    def add_required_message_fields
      # Override so we don't add Date, MIME-Version, or Message-ID.
    end

    def delivery_status_report_part?
      (main_type =~ /message/i && sub_type =~ /delivery-status/i) && body =~ /Status:/
    end

    # Returns the parsed message/delivery-status body of this part as a
    # Mail::DeliveryStatus, or nil if this part is not a delivery status
    # report. Exposes the message-level fields (Reporting-MTA, Arrival-Date,
    # ...) and one RecipientRecord per reported recipient.
    #
    # Raises Mail::DeliveryStatus::ParseError if the body is malformed, and
    # Mail::DeliveryStatus::MissingFieldError if a recipient record lacks a
    # field required by RFC 3464 (Final-Recipient, Action or Status).
    def delivery_status
      return nil unless delivery_status_report_part?

      unless defined? @delivery_status
        @delivery_status = DeliveryStatus.parse(body.decoded)
      end
      @delivery_status
    end

    def delivery_status_data
      delivery_status_report_part? ? parse_delivery_status_report : {}
    end

    # Returns true if any recipient record in the report has a permanent
    # failure (action: failed). A single report can cover several recipients
    # with mixed outcomes; bounced? is true as soon as at least one of them
    # permanently failed.
    def bounced?
      Array(action).any? { |a| a.to_s =~ /failed/i }
    end


    # Either returns the action if the message has just a single recipient
    # record, or an array of all the actions, one for each recipient record
    def action
      get_return_values('action')
    end

    def final_recipient
      get_return_values('final-recipient')
    end

    def error_status
      get_return_values('status')
    end

    def diagnostic_code
      get_return_values('diagnostic-code')
    end

    def remote_mta
      get_return_values('remote-mta')
    end

    # Returns true if the delivery failure may be transient. With several
    # recipient records, retryable? is true when at least one record has a
    # non-permanent (non 5.x.x) status.
    def retryable?
      statuses = Array(error_status)
      statuses.empty? || statuses.any? { |s| s.to_s !~ /^5/ }
    end

    private

    def get_return_values(key)
      if (status = structured_delivery_status)
        values = status.recipient_records.map { |record| record[key] }.compact
        values.length > 1 ? values : values.first
      else
        legacy_return_values(key)
      end
    end

    # Fallback for reports whose body cannot be strictly parsed: keep the
    # historical behaviour of merging every field group into a single header.
    def legacy_return_values(key)
      data = delivery_status_data
      if data[key].is_a?(Array)
        data[key].map { |a| a.value }
      elsif !data[key].nil?
        data[key].value
      else
        nil
      end
    end

    # Strictly parsed delivery status, or nil when the body does not follow
    # RFC 3464 (legacy single-value accessors fall back to the lenient
    # header merge in that case).
    def structured_delivery_status
      unless defined? @structured_delivery_status
        @structured_delivery_status =
          begin
            delivery_status
          rescue DeliveryStatus::ParseError
            nil
          end
      end
      @structured_delivery_status
    end

    # A part may not have a header.... so, just init a body if no header
    def parse_message
      header_part, body_part = raw_source.split(/#{Constants::LAX_CRLF}#{Constants::WSP}*#{Constants::LAX_CRLF}/m, 2)
      if header_part =~ Constants::HEADER_LINE
        self.header = header_part
        self.body   = body_part
      else
        self.header = "Content-Type: text/plain\r\n"
        self.body   = raw_source
      end
    end

    def parse_delivery_status_report
      @delivery_status_data ||= Header.new(body.to_s.gsub("\r\n\r\n", "\r\n"))
    end

  end

end
