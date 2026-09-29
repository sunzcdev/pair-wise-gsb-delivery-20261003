# frozen_string_literal: true
require 'mail/smtp_envelope'
require 'mail/smtp_delivery_result'

module Mail
  # == Sending Email with SMTP
  #
  # Mail allows you to send emails using an open SMTP connection.  This is done by
  # passing a created Net::SMTP object.  This way we can get better performance to
  # our local mail server by reducing the number of connections at any one time.
  #
  # === Sending via SMTP server on Localhost
  #
  # To send mail open a connection with Net::Smtp using any options you like
  # === Delivering the email
  #
  # Once you have the settings right, sending the email is done by:
  #
  #   smtp_conn = Net::SMTP.start(settings[:address], settings[:port])
  #   Mail.defaults do
  #     delivery_method :smtp_connection, { :connection => smtp_conn }
  #   end
  #
  #   Mail.deliver do
  #     to 'mikel@test.lindsaar.net'
  #     from 'ada@test.lindsaar.net'
  #     subject 'testing sendmail'
  #     body 'testing sendmail'
  #   end
  #
  # Or by calling deliver on a Mail message
  #
  #   mail = Mail.new do
  #     to 'mikel@test.lindsaar.net'
  #     from 'ada@test.lindsaar.net'
  #     subject 'testing sendmail'
  #     body 'testing sendmail'
  #   end
  #
  #   mail.deliver!
  class SMTPConnection
    attr_accessor :smtp, :settings

    def initialize(values)
      raise ArgumentError.new('A Net::SMTP object is required for this delivery method') if values[:connection].nil?
      self.smtp = values[:connection]
      self.settings = values
    end

    # Send the message via SMTP.
    # The from and to attributes are optional. If not set, they are retrieve from the Message.
    def deliver!(mail)
      envelope = Mail::SmtpEnvelope.new(mail)
      response = smtp.sendmail(envelope.message, envelope.from, envelope.to)
      settings[:return_response] ? response : self
    end

    # Send the message via SMTP, recipient by recipient, and return a
    # Mail::SMTPDeliveryResult describing the server's response for each
    # envelope recipient and whether the message DATA was successfully
    # submitted.
    #
    # Unlike #deliver!, a rejected recipient does not abort the delivery:
    # every envelope recipient (To, Cc and Bcc addresses) is attempted,
    # and DATA is submitted as long as at least one recipient was
    # accepted. Use the returned result to retry only the addresses that
    # were not delivered:
    #
    #   result = Mail.delivery_method.deliver_detailed!(mail)
    #   retry_later(result.retryable_to) unless result.success?
    #
    # If the connection is lost while submitting DATA, the result is
    # marked unknown (result.unknown?) instead of failed, because the
    # server may already have accepted the message. Such recipients are
    # excluded from result.retryable_to; do not auto-resend to them.
    #
    # A rejected MAIL FROM still raises, as no recipient can be
    # attempted without a valid reverse-path.
    def deliver_detailed!(mail)
      envelope = Mail::SmtpEnvelope.new(mail)
      result = Mail::SMTPDeliveryResult.new(envelope.from, envelope.to)

      smtp.mailfrom(envelope.from)

      envelope.to.each do |address|
        begin
          result.record_recipient_accepted(address, smtp.rcptto(address))
        rescue Net::SMTPError => error
          result.record_recipient_rejected(address, error)
        end
      end

      if result.recipients.any?(&:accepted?)
        begin
          result.record_data_success(smtp.data(envelope.message))
        rescue Net::SMTPError => error
          # The server gave a definitive answer: the message was not
          # accepted, so every accepted recipient is safe to retry.
          result.record_data_failure(error)
        rescue StandardError => error
          # The connection was lost during DATA. The server may or may
          # not have accepted the message, so the outcome is unknown:
          # do not treat it as a failure and auto-resend.
          result.record_data_unknown(error)
        end
      end

      result
    end
  end
end
