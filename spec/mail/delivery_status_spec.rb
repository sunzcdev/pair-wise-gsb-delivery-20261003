# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

RSpec.describe Mail::DeliveryStatus do

  def mixed_report_mail
    read_fixture('emails', 'multipart_report_emails', 'mixed_recipients_report.eml')
  end

  def mixed_report_status
    mixed_report_mail.delivery_status
  end

  describe "parsing a mixed three-recipient report" do
    it "is exposed on the message and on the delivery-status part" do
      mail = mixed_report_mail
      expect(mail.delivery_status).to be_a(described_class)
      expect(mail.delivery_status_part.delivery_status).to be_a(described_class)
    end

    it "parses the message-level fields" do
      status = mixed_report_status
      expect(status.reporting_mta).to eq 'dns; mail.example.net'
      expect(status.arrival_date).to eq 'Wed, 30 Sep 2026 09:14:32 +0800 (CST)'
    end

    it "keeps unknown message-level extensions" do
      expect(mixed_report_status.message_extensions).to eq(
        'X-Postfix-Queue-ID' => '4A1B2C3D4E',
        'X-Postfix-Sender'   => 'rfc822; sender@example.com'
      )
    end

    it "parses one record per recipient, in order" do
      records = mixed_report_status.recipient_records
      expect(records.length).to eq 3
      expect(records.map(&:final_recipient)).to eq [
        'rfc822; delivered@example.org',
        'rfc822; delayed@example.net',
        'rfc822; failed@example.com'
      ]
      expect(records.map(&:action)).to eq %w[delivered delayed failed]
      expect(records.map(&:status)).to eq %w[2.0.0 4.2.2 5.1.1]
    end

    it "unfolds folded diagnostic-code fields" do
      records = mixed_report_status.recipient_records
      expect(records[1].diagnostic_code).to eq \
        'smtp; 452 4.2.2 <delayed@example.net>... Mailbox full,    try again later'
      expect(records[2].diagnostic_code).to eq \
        'smtp; 550 5.1.1 <failed@example.com>... User unknown    in local recipient table'
    end

    it "keeps unknown per-recipient extensions on their own record" do
      records = mixed_report_status.recipient_records
      expect(records[1].extensions).to eq('X-Retry-Queue' => 'bulk')
      expect(records[0].extensions).to eq({})
      expect(records[2].extensions).to eq({})
    end

    it "answers per-recipient predicates" do
      delivered, delayed, failed = mixed_report_status.recipient_records
      expect(delivered).to be_delivered
      expect(delivered).not_to be_bounced
      expect(delayed).to be_delayed
      expect(delayed).to be_retryable
      expect(failed).to be_failed
      expect(failed).to be_bounced
      expect(failed).not_to be_retryable
    end
  end

  describe "re-encoding" do
    it "re-encodes to the original field groups" do
      mail = mixed_report_mail
      body = mail.delivery_status_part.body.decoded
      expect(mail.delivery_status.to_s).to eq body.strip.gsub(/\r?\n/, "\r\n")
    end

    it "round-trips through a second parse" do
      first = mixed_report_status
      second = described_class.parse(first.to_s)
      expect(second.message_fields).to eq first.message_fields
      expect(second.recipient_records).to eq first.recipient_records
      expect(second.to_s).to eq first.to_s
    end

    it "leaves the delivery-status part untouched when re-encoding the whole message" do
      mail = mixed_report_mail
      reparsed = Mail.new(mail.encoded)
      expect(reparsed.delivery_status.recipient_records.map(&:action)).to eq \
        %w[delivered delayed failed]
    end
  end

  describe "diagnostics" do
    it "raises a clear error when a recipient record misses a required field" do
      body = "Reporting-MTA: dns; mail.example.net\r\n" \
             "\r\n" \
             "Final-Recipient: rfc822; one@example.com\r\n" \
             "Action: failed\r\n" \
             "\r\n" \
             "Final-Recipient: rfc822; two@example.com\r\n" \
             "Action: failed\r\n" \
             "Status: 5.1.1\r\n"
      expect { described_class.parse(body) }.to raise_error(
        Mail::DeliveryStatus::MissingFieldError,
        /recipient record 1.*missing.*'status'/i
      )
    end

    it "does not backfill a missing field from the next recipient's record" do
      body = "Reporting-MTA: dns; mail.example.net\r\n" \
             "\r\n" \
             "Final-Recipient: rfc822; one@example.com\r\n" \
             "Action: failed\r\n" \
             "\r\n" \
             "Final-Recipient: rfc822; two@example.com\r\n" \
             "Action: failed\r\n" \
             "Status: 5.1.1\r\n"
      # record 1 has no Status even though record 2 does: it must raise
      # instead of borrowing record 2's Status.
      expect { described_class.parse(body) }.to raise_error(
        Mail::DeliveryStatus::MissingFieldError
      )
    end

    it "raises a clear error on a line that is not a field" do
      body = "Reporting-MTA: dns; mail.example.net\r\n" \
             "\r\n" \
             "Final-Recipient: rfc822; one@example.com\r\n" \
             "this is not a field\r\n"
      expect { described_class.parse(body) }.to raise_error(
        Mail::DeliveryStatus::ParseError,
        /recipient record 1.*not a 'Field-Name: value' field.*this is not a field/
      )
    end

    it "raises a clear error on an orphaned continuation line" do
      body = "Reporting-MTA: dns; mail.example.net\r\n" \
             "\r\n" \
             "    folded without a field\r\n"
      expect { described_class.parse(body) }.to raise_error(
        Mail::DeliveryStatus::ParseError,
        /recipient record 1.*continuation line/
      )
    end

    it "raises a clear error on an empty body" do
      expect { described_class.parse('') }.to raise_error(
        Mail::DeliveryStatus::ParseError, /empty/
      )
    end
  end

  describe "single-value accessors on a mixed report (backward compatibility)" do
    let(:mail) { mixed_report_mail }

    it "returns per-recipient arrays from the single-value accessors" do
      expect(mail.action).to eq %w[delivered delayed failed]
      expect(mail.error_status).to eq %w[2.0.0 4.2.2 5.1.1]
      expect(mail.final_recipient).to eq [
        'rfc822; delivered@example.org',
        'rfc822; delayed@example.net',
        'rfc822; failed@example.com'
      ]
    end

    it "is bounced when any recipient permanently failed" do
      expect(mail).to be_bounced
    end

    it "is retryable when any recipient has a non-permanent status" do
      expect(mail).to be_retryable
    end
  end

  describe "legacy fallback" do
    it "falls back to the lenient header merge for malformed reports" do
      part = Mail::Part.new do
        content_type 'message/delivery-status'
        body "Reporting-MTA: dns; mail.example.net\r\n" \
             "\r\n" \
             "Final-Recipient: rfc822; one@example.com\r\n" \
             "Action: failed\r\n" \
             "Status: 5.1.1\r\n" \
             "garbage line\r\n"
      end
      expect { part.delivery_status }.to raise_error(Mail::DeliveryStatus::ParseError)
      expect(part.action).to eq 'failed'
      expect(part).to be_bounced
    end
  end
end
