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

    def delivery_status_data
      delivery_status || {}
    end

    def delivery_status
      @delivery_status ||=
        if delivery_status_report_part?
          DeliveryStatus.parse(body.raw_source)
        end
    end

    def bounced?
      delivery_status && delivery_status.bounced?
    end

    # Either returns the action if the message has just a single report, or an
    # array of all the actions, one for each recipient. Legacy predicates remain
    # single-valued and represent the first recipient record; inspect
    # delivery_status.recipients for mixed reports.
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

    def retryable?
      delivery_status && delivery_status.retryable?
    end

    def encoded
      if delivery_status_report_part?
        @body = Mail::Body.new(delivery_status.encoded)
        @body.encoding = '7bit'
      end

      super
    end

    private

    def get_return_values(key)
      delivery_status_data[key]
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

  end

end
