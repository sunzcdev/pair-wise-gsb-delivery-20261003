# frozen_string_literal: true

module Mail
  # Detailed result of a single SMTP delivery attempt.
  #
  # Returned by Mail::SMTPConnection#deliver_detailed! and
  # Mail::SMTP#deliver_detailed!. Where the plain #deliver! interface
  # raises on the first rejected recipient, a detailed delivery always
  # attempts every envelope recipient and records the server's response
  # for each one, plus whether the message DATA was successfully
  # submitted. This lets the caller retry only the addresses that were
  # not delivered instead of resending to everyone.
  #
  #   result = Mail.delivery_method.deliver_detailed!(mail)
  #   result.retryable_to # => addresses safe to send again
  #
  # Bcc recipients are included: they are envelope recipients like any
  # other, even though they never appear in the message headers.
  #
  # One caveat: if the connection is lost while submitting DATA, the
  # server may or may not have accepted the message. That outcome is
  # reported as unknown (see #unknown? and #unknown_to) and those
  # recipients are deliberately excluded from #retryable_to, because
  # resending could deliver the message twice. Do not auto-resend on an
  # unknown result; reconcile with the server (e.g. via logs or queue
  # inspection) first.
  class SMTPDeliveryResult
    # Outcome of one envelope recipient (one RCPT TO command).
    class Recipient
      # The envelope recipient address.
      attr_reader :address

      # The Net::SMTP::Response returned by the server, or nil when the
      # recipient was rejected with an error instead.
      attr_reader :response

      # The Net::SMTPError raised for a rejected recipient, or nil when
      # the recipient was accepted.
      attr_reader :error

      def initialize(address, response, error) #:nodoc:
        @address = address
        @response = response
        @error = error
      end

      # True when the server accepted the RCPT TO command for this
      # recipient. Acceptance only means the server agreed to consider
      # the address; see SMTPDeliveryResult#status_of for the final
      # delivery outcome.
      def accepted?
        @error.nil?
      end

      # True when the server rejected the RCPT TO command for this
      # recipient.
      def rejected?
        !accepted?
      end
    end

    # The envelope sender address (MAIL FROM).
    attr_reader :from

    # All attempted envelope recipients, as SMTPDeliveryResult::Recipient
    # objects, in the order they were attempted.
    attr_reader :recipients

    # The Net::SMTP::Response confirming the message DATA, or nil when
    # DATA was not confirmed.
    attr_reader :data_response

    # The error raised while submitting DATA, or nil when DATA was
    # confirmed or never attempted.
    attr_reader :data_error

    def initialize(from, addresses) #:nodoc:
      @from = from
      @addresses = addresses
      @recipients = []
      @data_status = :not_attempted
    end

    def record_recipient_accepted(address, response) #:nodoc:
      @recipients << Recipient.new(address, response, nil)
    end

    def record_recipient_rejected(address, error) #:nodoc:
      @recipients << Recipient.new(address, nil, error)
    end

    def record_data_success(response) #:nodoc:
      @data_status = :succeeded
      @data_response = response
    end

    def record_data_failure(error) #:nodoc:
      @data_status = :failed
      @data_error = error
    end

    def record_data_unknown(error) #:nodoc:
      @data_status = :unknown
      @data_error = error
    end

    # Outcome of the DATA phase:
    #
    # [:succeeded]     the server confirmed it accepted the message.
    # [:failed]        the server gave a definitive refusal of the
    #                  message; nothing was delivered.
    # [:not_attempted] DATA was never sent, because no recipient was
    #                  accepted.
    # [:unknown]       the connection was lost while submitting DATA;
    #                  the server may or may not have accepted the
    #                  message.
    def data_status
      @data_status
    end

    # True when the server confirmed acceptance of the message DATA.
    def data_succeeded?
      @data_status == :succeeded
    end

    # True when the DATA outcome is unknown (connection lost during
    # DATA). The message may have been delivered; do not auto-resend.
    def unknown?
      @data_status == :unknown
    end

    # True when every envelope recipient was delivered.
    def success?
      !recipients.empty? && undelivered_recipients.empty?
    end

    # True when some, but not all, envelope recipients were delivered.
    def partial?
      !delivered_to.empty? && !undelivered_recipients.empty?
    end

    # The final status of one envelope recipient:
    #
    # [:delivered]   the server accepted the recipient and confirmed
    #                the message DATA.
    # [:rejected]    the server refused the RCPT TO command.
    # [:undelivered] the server accepted the recipient, but the message
    #                DATA was definitively not accepted. Safe to retry.
    # [:unknown]     the server accepted the recipient, but the
    #                connection was lost during DATA. The message may
    #                have been delivered; do not auto-resend.
    def status_of(address)
      recipient = recipients.find { |r| r.address == address } or
        raise ArgumentError, "no such envelope recipient: #{address.inspect}"

      return :rejected if recipient.rejected?

      case data_status
      when :succeeded then :delivered
      when :unknown   then :unknown
      else :undelivered
      end
    end

    # Addresses confirmed delivered: RCPT TO accepted and DATA
    # confirmed by the server.
    def delivered_to
      data_succeeded? ? accepted_addresses : []
    end

    # Addresses the server refused at RCPT TO time.
    def rejected_to
      recipients.select(&:rejected?).map(&:address)
    end

    # Addresses accepted at RCPT TO time whose final outcome is unknown
    # because the connection was lost during DATA. These may have been
    # delivered; do not auto-resend to them.
    def unknown_to
      unknown? ? accepted_addresses : []
    end

    # Addresses that were definitively not delivered and are safe to
    # retry: rejected recipients, plus accepted recipients when DATA
    # failed definitively or was never attempted. Recipients with an
    # unknown outcome are excluded on purpose.
    def retryable_to
      retryable = rejected_to
      retryable += accepted_addresses if data_status == :failed || data_status == :not_attempted
      retryable
    end

    private
      def accepted_addresses
        recipients.select(&:accepted?).map(&:address)
      end

      def undelivered_recipients
        recipients.reject { |r| status_of(r.address) == :delivered }
      end
  end
end
