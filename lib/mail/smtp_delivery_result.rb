# frozen_string_literal: true

module Mail
  # Detailed result of an SMTP delivery attempt, recording the server
  # response for each envelope recipient and the outcome of the DATA phase.
  #
  # Returned by Mail::SMTPConnection#deliver_with_result!. An SMTP server
  # may accept some recipients and reject others, so use this result to
  # find out which recipients still need the message and retry only those:
  #
  #   result = Mail.delivery_method.deliver_with_result!(mail)
  #   if result.data_unknown?
  #     # The connection dropped while submitting DATA. The server may or
  #     # may not have accepted the message for the accepted recipients,
  #     # so do not blindly resend to them.
  #   else
  #     result.undelivered_recipients # => addresses safe to retry
  #   end
  class SMTPDeliveryResult
    # The server response recorded for a single envelope recipient
    # (one RCPT TO command).
    class RecipientResponse
      attr_reader :address, :response, :error

      def initialize(address, response: nil, error: nil)
        @address = address
        @response = response
        @error = error
      end

      # True when the server accepted the RCPT TO command for this address.
      def accepted?
        @error.nil?
      end

      # True when the server rejected the RCPT TO command for this address.
      def rejected?
        !accepted?
      end
    end

    attr_reader :from, :recipient_responses, :data_response, :data_error

    def initialize(from:)
      @from = from
      @recipient_responses = []
      @data_status = :not_sent
    end

    def record_recipient_response(address, response: nil, error: nil)
      @recipient_responses << RecipientResponse.new(address, :response => response, :error => error)
    end

    # Records the outcome of the DATA phase. Without an error the server
    # accepted the message; a Net::SMTPError means the server explicitly
    # rejected it.
    def record_data_response(response: nil, error: nil)
      @data_response = response
      @data_error = error
      @data_status = error ? :rejected : :accepted
    end

    # Records that the connection dropped while submitting DATA. The
    # server may or may not have accepted the message, so the delivery
    # state of the accepted recipients is unknown.
    def record_data_unknown(error)
      @data_error = error
      @data_status = :unknown
    end

    # True when DATA was sent and the server accepted the message.
    def data_accepted?
      @data_status == :accepted
    end

    # True when DATA was sent and the server explicitly rejected the message.
    def data_rejected?
      @data_status == :rejected
    end

    # True when the connection dropped while submitting DATA. The message
    # may or may not have been accepted by the server.
    def data_unknown?
      @data_status == :unknown
    end

    # Envelope recipients the server accepted with RCPT TO.
    def accepted_recipients
      @recipient_responses.select(&:accepted?).map(&:address)
    end

    # Envelope recipients the server rejected with RCPT TO.
    def rejected_recipients
      @recipient_responses.select(&:rejected?).map(&:address)
    end

    # True when the server accepted the message for every envelope recipient.
    def delivered?
      data_accepted? && rejected_recipients.empty?
    end

    # Recipients that definitely did not receive the message and are safe
    # to retry. Accepted recipients are excluded when the DATA outcome is
    # unknown, since the server may already have the message.
    def undelivered_recipients
      if data_accepted? || data_unknown?
        rejected_recipients
      else
        @recipient_responses.map(&:address)
      end
    end

    # Recipients whose delivery state is unknown because the connection
    # dropped during DATA. Do not resend to these addresses without
    # reconciling with the server first.
    def unknown_recipients
      data_unknown? ? accepted_recipients : []
    end
  end
end
