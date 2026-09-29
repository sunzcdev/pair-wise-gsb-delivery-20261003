# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

# A fake SMTP connection that returns scripted responses step by step,
# emulating the low-level Net::SMTP conversation (MAIL FROM, RCPT TO per
# recipient, DATA) without sending any real email.
class ScriptedSMTPConnection
  attr_reader :commands

  def initialize
    @commands = []
    @rcpt_errors = {}
    @data_error = nil
  end

  def reject_recipient(address, message = "550 5.1.1 No such user")
    @rcpt_errors[address] = Net::SMTPFatalError.new(message, :message => message)
  end

  def fail_data_with(error)
    @data_error = error
  end

  def mailfrom(from)
    @commands << [:mailfrom, from]
    "250 2.1.0 Sender OK"
  end

  def rcptto(to)
    @commands << [:rcptto, to]
    error = @rcpt_errors[to]
    raise error if error
    "250 2.1.5 Recipient OK"
  end

  def data(message)
    @commands << [:data, message]
    raise @data_error if @data_error
    "250 2.0.0 Message accepted for delivery"
  end

  # Emulates Net::SMTP#sendmail on top of the scripted conversation.
  def sendmail(message, from, to)
    mailfrom(from)
    to.each { |addr| rcptto(addr) }
    data(message)
  end
end

RSpec.describe "SMTP detailed delivery result" do

  let(:connection) { ScriptedSMTPConnection.new }
  let(:delivery_method) { Mail::SMTPConnection.new(:connection => connection) }

  def build_mail(options = {})
    Mail.new do
      from    'sender@example.com'
      to      options[:to] || 'accepted@example.com'
      subject 'detailed delivery result'
      body    'testing'
    end
  end

  def rcptto_commands
    connection.commands.select { |command, _| command == :rcptto }.map(&:last)
  end

  def data_commands
    connection.commands.select { |command, _| command == :data }
  end

  describe "when the server accepts some recipients and rejects others" do
    it "records each recipient response and only retries the rejected ones" do
      connection.reject_recipient('rejected@example.com')

      mail = build_mail(:to => 'accepted@example.com, rejected@example.com')
      result = delivery_method.deliver_with_result!(mail)

      expect(rcptto_commands).to eq(%w(accepted@example.com rejected@example.com))
      expect(data_commands.size).to eq(1)

      expect(result.accepted_recipients).to eq(%w(accepted@example.com))
      expect(result.rejected_recipients).to eq(%w(rejected@example.com))
      expect(result.undelivered_recipients).to eq(%w(rejected@example.com))
      expect(result.unknown_recipients).to eq([])

      expect(result.data_accepted?).to be(true)
      expect(result.data_unknown?).to be(false)
      expect(result.delivered?).to be(false)

      accepted, rejected = result.recipient_responses
      expect(accepted.address).to eq('accepted@example.com')
      expect(accepted.response).to eq('250 2.1.5 Recipient OK')
      expect(accepted).to be_accepted
      expect(rejected.address).to eq('rejected@example.com')
      expect(rejected.error).to be_a(Net::SMTPError)
      expect(rejected.error.message).to eq('550 5.1.1 No such user')
      expect(rejected).to be_rejected
    end
  end

  describe "when the server rejects every recipient" do
    it "never submits DATA and reports all recipients as undelivered" do
      connection.reject_recipient('first@example.com')
      connection.reject_recipient('second@example.com', "554 5.7.1 Relay access denied")

      mail = build_mail(:to => 'first@example.com, second@example.com')
      result = delivery_method.deliver_with_result!(mail)

      expect(rcptto_commands).to eq(%w(first@example.com second@example.com))
      expect(data_commands).to be_empty

      expect(result.accepted_recipients).to eq([])
      expect(result.rejected_recipients).to eq(%w(first@example.com second@example.com))
      expect(result.undelivered_recipients).to eq(%w(first@example.com second@example.com))

      expect(result.data_accepted?).to be(false)
      expect(result.data_unknown?).to be(false)
      expect(result.delivered?).to be(false)
    end
  end

  describe "when the connection drops after DATA is submitted" do
    it "reports the outcome as unknown instead of failed" do
      connection.reject_recipient('rejected@example.com')
      connection.fail_data_with(EOFError.new('end of file reached'))

      mail = build_mail(:to => 'accepted@example.com, rejected@example.com')
      result = delivery_method.deliver_with_result!(mail)

      expect(data_commands.size).to eq(1)

      expect(result.data_unknown?).to be(true)
      expect(result.data_accepted?).to be(false)
      expect(result.data_error).to be_a(EOFError)

      # The accepted recipient may already have the message: it must not
      # be reported as undelivered and resent automatically.
      expect(result.unknown_recipients).to eq(%w(accepted@example.com))
      expect(result.undelivered_recipients).to eq(%w(rejected@example.com))
      expect(result.delivered?).to be(false)
    end
  end

  describe "when the server rejects the message after DATA" do
    it "reports every recipient as undelivered" do
      message = "452 4.3.1 Insufficient system storage"
      connection.fail_data_with(Net::SMTPServerBusy.new(message, :message => message))

      mail = build_mail(:to => 'accepted@example.com')
      result = delivery_method.deliver_with_result!(mail)

      expect(result.data_rejected?).to be(true)
      expect(result.data_unknown?).to be(false)
      expect(result.undelivered_recipients).to eq(%w(accepted@example.com))
      expect(result.delivered?).to be(false)
    end
  end

  describe "when everything succeeds" do
    it "reports the message as delivered" do
      mail = build_mail(:to => 'one@example.com, two@example.com')
      result = delivery_method.deliver_with_result!(mail)

      expect(result.delivered?).to be(true)
      expect(result.data_accepted?).to be(true)
      expect(result.data_response).to eq('250 2.0.0 Message accepted for delivery')
      expect(result.undelivered_recipients).to eq([])
      expect(result.from).to eq('sender@example.com')
    end
  end

  describe "Bcc recipients" do
    it "are delivered through the envelope only" do
      mail = Mail.new do
        from    'sender@example.com'
        to      'to@example.com'
        bcc     'hidden@example.com'
        subject 'bcc stays in the envelope'
        body    'testing'
      end

      result = delivery_method.deliver_with_result!(mail)

      expect(rcptto_commands).to eq(%w(to@example.com hidden@example.com))
      expect(result.recipient_responses.map(&:address)).to eq(%w(to@example.com hidden@example.com))
      expect(result.delivered?).to be(true)

      expect(mail.to).to eq(%w(to@example.com))
      expect(mail.cc).to be_nil
    end
  end

  describe "the original deliver! interface" do
    it "is still available and unchanged" do
      smtp_connection = connection
      Mail.defaults do
        delivery_method :smtp_connection, :connection => smtp_connection
      end

      mail = build_mail
      expect(mail.deliver!).to eq(mail)
      expect(connection.commands.map(&:first)).to eq([:mailfrom, :rcptto, :data])
    end
  end
end
