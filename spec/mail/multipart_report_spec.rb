# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

RSpec.describe "multipart/report emails" do
  
  it "should know if it is a multipart report type" do
    mail = read_fixture('emails', 'multipart_report_emails', 'report_422.eml')
    expect(mail).to be_multipart_report
  end

  describe "delivery-status reports" do

    it "should know if it is a deliver-status report" do
      mail = read_fixture('emails', 'multipart_report_emails', 'report_422.eml')
      expect(mail).to be_delivery_status_report
    end

    it "should find its message/delivery-status part" do
      mail = read_fixture('emails', 'multipart_report_emails', 'report_422.eml')
      expect(mail.delivery_status_part).not_to be_nil
    end

    it "should handle a report that has a human readable message/delivery-status" do
      mail = read_fixture('emails', 'multipart_report_emails', 'multipart_report_multiple_status.eml')
      expect(mail).to be_bounced
    end

    describe "delivery-status part in a non report email" do
      let(:mail){
        string = read_raw_fixture('emails', 'multipart_report_emails', 'report_422.eml')
        Mail.read_from_string(string.sub("multipart/report; report-type=delivery-status;", "multipart/mixed;"))
      }

      it "does not return a delivery_status_part" do
        expect(mail.delivery_status_part).to be_nil
      end
    end

    describe "multipart reports with more than one address" do
      it "should not crash" do
        mail1 = read_fixture('emails', 'multipart_report_emails', 'multi_address_bounce1.eml')
        mail2 = read_fixture('emails', 'multipart_report_emails', 'multi_address_bounce2.eml')
        expect { mail1.bounced? }.not_to raise_error
        expect { mail2.bounced? }.not_to raise_error
      end

      it "should not know that a multi address email was bounced" do
        mail1 = read_fixture('emails', 'multipart_report_emails', 'multi_address_bounce1.eml')
        mail2 = read_fixture('emails', 'multipart_report_emails', 'multi_address_bounce2.eml')
        expect(mail1).to be_bounced
        expect(mail2).to be_bounced
      end
    end

    describe "mixed recipient results" do
      let(:mail) do
        read_fixture('emails', 'multipart_report_emails', 'mixed_recipient_delivery_status.eml')
      end

      let(:delivery_status) { mail.delivery_status }

      it "parses message-level fields" do
        expect(delivery_status.message_fields['reporting-mta']).to eq 'dns; mta.example.com'
        expect(delivery_status.message_fields['x-generated-by']).to eq 'example-mta'
        expect(delivery_status.reporting_mta).to eq 'dns; mta.example.com'
      end

      it "parses one record for each recipient" do
        expect(delivery_status.recipients.size).to eq 3
        expect(delivery_status.action).to eq ['delivered', 'delayed', 'failed']
        expect(delivery_status.error_status).to eq ['2.0.0', '4.2.2', '5.1.1']
        expect(delivery_status.final_recipient).to eq [
          'rfc822; success@example.net',
          'rfc822; delayed@example.org',
          'rfc822; failed@example.invalid'
        ]
      end

      it "preserves diagnostic codes, known fields, and unknown extensions per record" do
        expect(delivery_status.diagnostic_code).to eq [
          'smtp; 250 2.0.0 Message accepted for delivery',
          'smtp; 452 4.2.2 Mailbox full; delivery will be retried later by the relay',
          'smtp; 550 5.1.1 Recipient address does not exist and cannot be retried by the sending system'
        ]
        expect(delivery_status.remote_mta).to eq [
          'dns; mx.example.net',
          'dns; mx.example.org',
          'dns; mx.example.invalid'
        ]
        expect(delivery_status.recipients.map { |record| record['x-delivery-queue'] || record['x-retry-after'] || record['x-failure-code'] }).to eq [
          'outgoing',
          '1800',
          'recipient-unknown'
        ]
        expect(delivery_status.message_fields.to_h['x-generated-by']).to eq 'example-mta'
        expect(delivery_status.recipients.first.to_h['x-delivery-queue']).to eq 'outgoing'
      end

      it "keeps each recipient's fields in separate records" do
        delivery_status.recipients.each do |record|
          expect(record.fields.map(&:name).grep(/\Afinal-recipient\z/i).size).to eq 1
          expect(record.fields.map(&:name).grep(/\Aaction\z/i).size).to eq 1
          expect(record.fields.map(&:name).grep(/\Astatus\z/i).size).to eq 1
        end
      end

      it "re-encodes records, folded fields, and unknown extensions" do
        encoded = delivery_status.encoded
        reparsed = Mail::DeliveryStatus.parse(encoded)

        expect(encoded).to include(
          "X-Generated-By: example-mta\r\n",
          "Diagnostic-Code: smtp; 452 4.2.2 Mailbox full; delivery will be retried later\r\n by the relay\r\n",
          "X-Failure-Code: recipient-unknown\r\n"
        )
        expect(reparsed.action).to eq delivery_status.action
        expect(reparsed.diagnostic_code).to eq delivery_status.diagnostic_code
        expect(reparsed.recipients.map { |record| record.fields.map { |field| [field.name, field.value] } }).to eq(
          delivery_status.recipients.map { |record| record.fields.map { |field| [field.name, field.value] } }
        )
      end

      it "unfolds continuation fields without crossing recipient boundaries" do
        source = "Reporting-MTA: dns; example.com\r\n\r\n" \
                 "Final-Recipient: rfc822; first@example.com\r\n" \
                 "Action: delayed\r\n" \
                 "Status: 4.2.2\r\n" \
                 "Diagnostic-Code: smtp; 452 try\r\n" \
                 "\tagain\r\n\r\n" \
                 "Final-Recipient: rfc822; second@example.com\r\n" \
                 "Action: failed\r\n" \
                 "Status: 5.1.1\r\n" \
                 "Diagnostic-Code: smtp; 550 gone\r\n"
        delivery_status = Mail::DeliveryStatus.parse(source)

        expect(delivery_status.recipients.first['diagnostic-code']).to eq "smtp; 452 try\tagain"
        expect(delivery_status.recipients.last['diagnostic-code']).to eq 'smtp; 550 gone'
        expect(Mail::DeliveryStatus.parse(delivery_status.encoded).status).to eq ['4.2.2', '5.1.1']
      end

      it "re-encodes the message/delivery-status part" do
        part = mail.delivery_status_part

        expect(part.encoded).to include(
          "Content-Type: message/delivery-status\r\n",
          "Action: delivered\r\n",
          "Action: delayed\r\n",
          "Action: failed\r\n"
        )
        expect(Mail.read_from_string(mail.encoded).delivery_status.action).to eq delivery_status.action
      end

      it "keeps legacy boolean methods compatible with the first recipient" do
        expect(mail.bounced?).to eq false
        expect(mail.retryable?).to eq true
        expect(delivery_status.recipients.map(&:bounced?)).to eq [false, false, true]
        expect(delivery_status.recipients.map(&:retryable?)).to eq [true, true, false]
      end
    end

    describe "malformed delivery status reports" do
      it "reports a field continuation without a preceding field" do
        source = "Reporting-MTA: dns; example.com\r\n\r\n orphaned\r\n"

        expect { Mail::DeliveryStatus.parse(source) }.to raise_error(
          Mail::DeliveryStatus::ParseError,
          /continuation on line 3/
        )
      end

      it "reports the line number of a malformed field inside a later record" do
        source = "Reporting-MTA: dns; example.com\r\n\r\n" \
                 "Final-Recipient: rfc822; first@example.com\r\n" \
                 "Action: delivered\r\n" \
                 "Status: 2.0.0\r\n\r\n" \
                 "Final-Recipient: rfc822; second@example.com\r\n" \
                 "Action delivered\r\n" \
                 "Status: 5.1.1\r\n"

        expect { Mail::DeliveryStatus.parse(source) }.to raise_error(
          Mail::DeliveryStatus::ParseError,
          /malformed delivery status field on line 8/
        )
      end

      it "reports the missing message-level required field" do
        source = "Arrival-Date: Tue, 30 Sep 2025\r\n\r\n" \
                 "Final-Recipient: rfc822; user@example.com\r\n" \
                 "Action: failed\r\n" \
                 "Status: 5.1.1\r\n"

        expect { Mail::DeliveryStatus.parse(source) }.to raise_error(
          Mail::DeliveryStatus::ParseError,
          /message .* missing required field "reporting-mta"/
        )
      end

      it "reports a report without a recipient field group" do
        source = "Reporting-MTA: dns; example.com\r\n"

        expect { Mail::DeliveryStatus.parse(source) }.to raise_error(
          Mail::DeliveryStatus::ParseError,
          /at least one recipient field group/
        )
      end

      it "reports which recipient record is missing a required field" do
        source = "Reporting-MTA: dns; example.com\r\n\r\n" \
                 "Final-Recipient: rfc822; first@example.com\r\n" \
                 "Action: delivered\r\n" \
                 "Status: 2.0.0\r\n\r\n" \
                 "Final-Recipient: rfc822; second@example.com\r\n" \
                 "Status: 5.1.1\r\n"

        expect { Mail::DeliveryStatus.parse(source) }.to raise_error(
          Mail::DeliveryStatus::ParseError,
          /recipient 2 .* missing required field "action"/
        )
      end

      it "does not use the next recipient's field for a missing value" do
        source = "Reporting-MTA: dns; example.com\r\n\r\n" \
                 "Final-Recipient: rfc822; first@example.com\r\n" \
                 "Action: failed\r\n\r\n" \
                 "Final-Recipient: rfc822; second@example.com\r\n" \
                 "Action: delivered\r\n" \
                 "Status: 2.0.0\r\n"

        expect { Mail::DeliveryStatus.parse(source) }.to raise_error(
          Mail::DeliveryStatus::ParseError,
          /recipient 1 .* missing required field "status"/
        )
      end
    end

    describe "temporary failure" do

      before(:each) do
        @mail = read_fixture('emails', 'multipart_report_emails', 'report_422.eml')
      end

      it "should be bounced" do
        expect(@mail).not_to be_bounced
      end

      it "should say action 'delayed'" do
        expect(@mail.action).to eq 'delayed'
      end

      it "should give a final recipient" do
        expect(@mail.final_recipient).to eq 'RFC822; fraser@oooooooo.com.au'
      end

      it "should give an error code" do
        expect(@mail.error_status).to eq '4.2.2'
      end

      it "should give a diagostic code" do
        expect(@mail.diagnostic_code).to eq 'SMTP; 452 4.2.2 <fraser@oooooooo.com.au>... Mailbox full'
      end

      it "should give a remote-mta" do
        expect(@mail.remote_mta).to eq 'DNS; mail.oooooooo.com.au'
      end

      it "should be retryable" do
        expect(@mail).to be_retryable
      end
    end

    describe "permanent failure" do

      before(:each) do
        @mail = read_fixture('emails', 'multipart_report_emails', 'report_530.eml')
      end

      it "should be bounced" do
        expect(@mail).to be_bounced
      end

      it "should say action 'failed'" do
        expect(@mail.action).to eq 'failed'
      end

      it "should give a final recipient" do
        expect(@mail.final_recipient).to eq 'RFC822; edwin@zzzzzzz.com'
      end

      it "should give an error code" do
        expect(@mail.error_status).to eq '5.3.0'
      end

      it "should give a diagostic code" do
        expect(@mail.diagnostic_code).to eq 'SMTP; 553 5.3.0 <edwin@zzzzzzz.com>... Unknown E-Mail Address'
      end

      it "should give a remote-mta" do
        expect(@mail.remote_mta).to eq 'DNS; mail.zzzzzz.com'
      end

      it "should be retryable" do
        expect(@mail).not_to be_retryable
      end
    end

  end

end
