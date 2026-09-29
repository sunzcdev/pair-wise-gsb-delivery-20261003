# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

# A fake SMTP server connection that plays back scripted responses,
# command by command. It records every command it receives so specs can
# assert on the exact SMTP dialogue. No real email is sent.
class ScriptedSMTPConnection
  attr_reader :commands
  attr_accessor :open_timeout, :read_timeout

  # rcpt:         hash of address => :accept | :reject
  # rcpt_default: behavior for addresses not listed in rcpt
  # mailfrom:     :accept | :reject
  # data:         :success | :failure | :disconnect
  def initialize(rcpt: {}, rcpt_default: :accept, mailfrom: :accept, data: :success)
    @rcpt_behaviors = rcpt
    @rcpt_default = rcpt_default
    @mailfrom_behavior = mailfrom
    @data_behavior = data
    @commands = []
  end

  # Net::SMTP session interface used by Mail::SMTP
  def start(*_args)
    yield self
  end

  def disable_tls; end
  def enable_tls(*); end
  def disable_starttls; end
  def enable_starttls(*); end
  def enable_starttls_auto(*); end

  def mailfrom(address)
    @commands << [:mailfrom, address]
    if @mailfrom_behavior == :reject
      raise Net::SMTPFatalError, "553 5.1.7 <#{address}>: sender rejected"
    end
    smtp_response('250', '2.1.0 OK')
  end

  def rcptto(address)
    @commands << [:rcptto, address]
    case @rcpt_behaviors.fetch(address, @rcpt_default)
    when :accept
      smtp_response('250', '2.1.5 OK')
    when :reject
      raise Net::SMTPFatalError, "550 5.1.1 <#{address}>: recipient rejected"
    end
  end

  def data(message)
    @commands << [:data, message]
    case @data_behavior
    when :success
      smtp_response('250', '2.0.0 Ok: queued as 12345')
    when :failure
      raise Net::SMTPFatalError, '554 5.0.0 message refused'
    when :disconnect
      raise EOFError, 'end of file reached'
    end
  end

  private
    def smtp_response(status, message)
      Net::SMTP::Response.new(status, message)
    end
end

RSpec.describe "SMTP detailed delivery result" do
  def build_mail
    Mail.new do
      from    'sender@example.com'
      to      'accepted@example.com, rejected@example.com'
      bcc     'blind@example.com'
      subject 'detailed delivery'
      body    'hello'
    end
  end

  def deliver_with(connection, mail = build_mail)
    Mail::SMTPConnection.new(:connection => connection).deliver_detailed!(mail)
  end

  it "records each recipient response and confirms DATA when some recipients are rejected" do
    connection = ScriptedSMTPConnection.new(:rcpt => { 'rejected@example.com' => :reject })
    result = deliver_with(connection)

    expect(result).to be_a Mail::SMTPDeliveryResult
    expect(result.from).to eq 'sender@example.com'
    expect(result.data_status).to eq :succeeded
    expect(result.data_response.status).to eq '250'

    expect(result.success?).to be false
    expect(result.partial?).to be true
    expect(result.unknown?).to be false

    expect(result.delivered_to).to eq %w(accepted@example.com blind@example.com)
    expect(result.rejected_to).to eq %w(rejected@example.com)
    expect(result.unknown_to).to eq []
    expect(result.retryable_to).to eq %w(rejected@example.com)

    expect(result.status_of('accepted@example.com')).to eq :delivered
    expect(result.status_of('blind@example.com')).to eq :delivered
    expect(result.status_of('rejected@example.com')).to eq :rejected

    accepted = result.recipients.find { |r| r.address == 'accepted@example.com' }
    expect(accepted).to be_accepted
    expect(accepted.response.status).to eq '250'

    rejected = result.recipients.find { |r| r.address == 'rejected@example.com' }
    expect(rejected).to be_rejected
    expect(rejected.error).to be_a Net::SMTPError
    expect(rejected.error.message).to include '550'
  end

  it "keeps Bcc recipients in the envelope only, never in the message" do
    connection = ScriptedSMTPConnection.new(:rcpt => { 'rejected@example.com' => :reject })
    deliver_with(connection)

    rcpttos = connection.commands.select { |command, _| command == :rcptto }.map(&:last)
    expect(rcpttos).to eq %w(accepted@example.com rejected@example.com blind@example.com)

    data_command = connection.commands.find { |command, _| command == :data }
    expect(data_command).not_to be_nil
    expect(data_command.last).not_to include 'blind@example.com'
    expect(data_command.last).not_to match(/^Bcc:/i)
  end

  it "skips DATA and marks every recipient retryable when all recipients are rejected" do
    connection = ScriptedSMTPConnection.new(:rcpt_default => :reject)
    result = deliver_with(connection)

    expect(result.data_status).to eq :not_attempted
    expect(connection.commands.map(&:first)).not_to include :data

    expect(result.success?).to be false
    expect(result.partial?).to be false
    expect(result.delivered_to).to eq []
    expect(result.unknown_to).to eq []
    expect(result.retryable_to).to eq %w(accepted@example.com rejected@example.com blind@example.com)
    expect(result.status_of('blind@example.com')).to eq :rejected
  end

  it "reports an unknown outcome instead of a failure when the connection drops after DATA" do
    connection = ScriptedSMTPConnection.new(
      :rcpt => { 'rejected@example.com' => :reject },
      :data => :disconnect
    )

    result = nil
    expect { result = deliver_with(connection) }.not_to raise_error

    expect(result.unknown?).to be true
    expect(result.data_status).to eq :unknown
    expect(result.data_error).to be_a EOFError

    expect(result.success?).to be false
    expect(result.delivered_to).to eq []
    expect(result.unknown_to).to eq %w(accepted@example.com blind@example.com)
    expect(result.status_of('accepted@example.com')).to eq :unknown

    # Unknown recipients may already have the message, so only the
    # definitively rejected address is safe to retry.
    expect(result.retryable_to).to eq %w(rejected@example.com)
  end

  it "marks accepted recipients retryable when the server definitively refuses DATA" do
    connection = ScriptedSMTPConnection.new(:data => :failure)
    result = deliver_with(connection)

    expect(result.data_status).to eq :failed
    expect(result.unknown?).to be false
    expect(result.delivered_to).to eq []
    expect(result.retryable_to).to eq %w(accepted@example.com rejected@example.com blind@example.com)
    expect(result.status_of('accepted@example.com')).to eq :undelivered
  end

  it "reports success when every recipient is accepted and DATA is confirmed" do
    connection = ScriptedSMTPConnection.new
    result = deliver_with(connection)

    expect(result.success?).to be true
    expect(result.partial?).to be false
    expect(result.delivered_to).to eq %w(accepted@example.com rejected@example.com blind@example.com)
    expect(result.retryable_to).to eq []
  end

  it "raises when the server rejects MAIL FROM" do
    connection = ScriptedSMTPConnection.new(:mailfrom => :reject)
    expect { deliver_with(connection) }.to raise_error(Net::SMTPFatalError)
  end

  it "delivers through Mail::SMTP within a started session" do
    connection = ScriptedSMTPConnection.new(:rcpt => { 'rejected@example.com' => :reject })
    allow(Net::SMTP).to receive(:new).and_return(connection)

    result = Mail::SMTP.new(:address => 'smtp.example.com').deliver_detailed!(build_mail)

    expect(result.partial?).to be true
    expect(result.delivered_to).to eq %w(accepted@example.com blind@example.com)
    expect(result.retryable_to).to eq %w(rejected@example.com)
  end
end
