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

    # Send the message via SMTP and return a Mail::SMTPDeliveryResult
    # describing the server response for each envelope recipient and the
    # DATA phase.
    #
    # Unlike #deliver!, a rejected recipient does not raise; the rejection
    # is recorded in the result and delivery continues with the remaining
    # recipients, so callers can retry only the addresses that were not
    # delivered. DATA is only submitted when at least one recipient was
    # accepted.
    #
    # If the connection drops while submitting DATA, the result is marked
    # as unknown instead of failed: the server may still have accepted the
    # message, so accepted recipients must not be resent automatically.
    def deliver_with_result!(mail)
      envelope = Mail::SmtpEnvelope.new(mail)
      result = Mail::SMTPDeliveryResult.new(:from => envelope.from)

      smtp.mailfrom(envelope.from)

      envelope.to.each do |to|
        begin
          response = smtp.rcptto(to)
          result.record_recipient_response(to, :response => response)
        rescue Net::SMTPError => error
          result.record_recipient_response(to, :error => error)
        end
      end

      if result.accepted_recipients.any?
        begin
          response = smtp.data(envelope.message)
          result.record_data_response(:response => response)
        rescue Net::SMTPError => error
          result.record_data_response(:error => error)
        rescue IOError, SystemCallError, SocketError, EOFError, Timeout::Error => error
          result.record_data_unknown(error)
        end
      end

      result
    end
  end
end
