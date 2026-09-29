# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

RSpec.describe Mail::EncodingPlan do
  let(:plain_mail) { read_fixture('emails/plain_emails/raw_email.eml') }
  let(:utf8_mail)  { read_fixture('emails/rfc6532/utf8_headers.eml') }
  let(:eight_bit_mail) { read_fixture('emails/multi_charset/japanese_shift_jis.eml') }

  def plan_for(mail, capabilities)
    Mail::EncodingPlan.new(mail, capabilities: capabilities)
  end

  describe "capability handling" do
    it "normalizes capabilities to uppercase strings" do
      plan = plan_for(plain_mail, [:smtputf8, '8bitmime'])
      expect(plan.capabilities).to eq %w[SMTPUTF8 8BITMIME]
      expect(plan.offers?('smtputf8')).to be true
      expect(plan.offers?('BINARYMIME')).to be false
    end
  end

  describe "a plain 7bit message" do
    it "is deliverable with no capabilities and needs nothing" do
      plan = plan_for(plain_mail, [])
      expect(plan.deliverable?).to be true
      expect(plan.items).to be_empty
      expect(plan.required_capabilities).to be_empty
      expect(plan.missing_capabilities).to be_empty
    end
  end

  describe "a message with non-ASCII envelope addresses and headers" do
    it "blocks on the envelope without SMTPUTF8" do
      plan = plan_for(utf8_mail, [])
      expect(plan.deliverable?).to be false
      expect(plan.required_capabilities).to eq %w[SMTPUTF8]
      expect(plan.missing_capabilities).to eq %w[SMTPUTF8]

      blockers = plan.blockers
      expect(blockers.map(&:scope)).to eq [:envelope, :envelope]
      expect(blockers.map(&:subject)).to contain_exactly('jdöe@mächine.example', 'märy@exämple.net')
      expect(blockers.map(&:capability)).to eq %w[SMTPUTF8 SMTPUTF8]
      expect(blockers.map(&:detail)).to all(match(/cannot be rewritten/))
    end

    it "plans lossless header re-encoding instead of downgrading addresses" do
      plan = plan_for(utf8_mail, [])
      adjustments = plan.adjustments
      expect(adjustments.map(&:scope)).to eq [:header, :header, :header]
      expect(adjustments.map(&:subject)).to contain_exactly('message header From', 'message header To', 'message header Subject')
      expect(adjustments.map(&:detail)).to all(match(/losslessly re-encoded/))
    end

    it "is deliverable as-is with SMTPUTF8" do
      plan = plan_for(utf8_mail, %w[SMTPUTF8])
      expect(plan.deliverable?).to be true
      expect(plan.blockers).to be_empty
      expect(plan.missing_capabilities).to be_empty
      expect(plan.items.select(&:ok?).map(&:scope)).to include(:envelope, :header)
    end

    it "refuses to apply a blocked plan" do
      plan = plan_for(utf8_mail, [])
      expect { plan.apply }.to raise_error(Mail::EncodingPlan::UnsatisfiableError) do |error|
        expect(error.blockers).to eq plan.blockers
      end
    end

    it "never rewrites the internationalized addresses when applying" do
      plan = plan_for(utf8_mail, %w[SMTPUTF8])
      applied = plan.apply
      expect(applied.smtp_envelope_from).to eq utf8_mail.smtp_envelope_from
      expect(applied.smtp_envelope_to).to eq utf8_mail.smtp_envelope_to
    end
  end

  describe "a message with an 8bit body" do
    it "requires 8BITMIME to be sent as-is" do
      plan = plan_for(eight_bit_mail, %w[8BITMIME])
      expect(plan.deliverable?).to be true
      expect(plan.adjustments).to be_empty
      expect(plan.required_capabilities).to eq %w[8BITMIME]
      item = plan.items.find { |i| i.scope == :body }
      expect(item.status).to eq :ok
      expect(item.capability).to eq '8BITMIME'
    end

    it "plans a lossless re-encoding without 8BITMIME" do
      plan = plan_for(eight_bit_mail, [])
      expect(plan.deliverable?).to be true
      expect(plan.missing_capabilities).to eq %w[8BITMIME]

      item = plan.adjustments.find { |i| i.scope == :body }
      expect(item.encoding).to eq 'quoted-printable'
      expect(item.capability).to eq '8BITMIME'
    end

    it "applies the re-encoding to a copy, leaving the original untouched" do
      before = eight_bit_mail.encoded
      plan = plan_for(eight_bit_mail, [])

      applied = plan.apply
      expect(applied).not_to be eight_bit_mail
      expect(applied.encoded).to satisfy { |encoded| encoded.ascii_only? }
      expect(applied.content_transfer_encoding).to eq 'quoted-printable'

      # decoded content survives the round trip
      expect(Mail.new(applied.encoded).body.decoded).to eq eight_bit_mail.body.decoded

      # the original message is unchanged
      expect(eight_bit_mail.encoded).to eq before
      expect(eight_bit_mail.body.encoding).to eq '8bit'
    end
  end

  describe "a message with a binary body" do
    let(:binary_mail) do
      Mail.new.tap do |mail|
        mail.from = 'sender@example.com'
        mail.to = 'rcpt@example.com'
        mail.content_type = 'application/octet-stream'
        mail.body = "bin\x00ary\xFF\xFE content".b
      end
    end

    it "requires BINARYMIME to be sent as-is" do
      plan = plan_for(binary_mail, %w[8BITMIME BINARYMIME])
      expect(plan.deliverable?).to be true
      expect(plan.adjustments).to be_empty
      item = plan.items.find { |i| i.scope == :body }
      expect(item.status).to eq :ok
      expect(item.capability).to eq 'BINARYMIME'
    end

    it "falls back to base64 when only 8BITMIME is offered" do
      plan = plan_for(binary_mail, %w[8BITMIME])
      expect(plan.deliverable?).to be true
      item = plan.adjustments.find { |i| i.scope == :body }
      expect(item.encoding).to eq 'base64'
      expect(item.capability).to eq 'BINARYMIME'

      applied = plan.apply
      expect(applied.encoded).to satisfy { |encoded| encoded.ascii_only? }
      expect(Mail.new(applied.encoded).body.decoded).to eq binary_mail.body.decoded
    end
  end

  describe "a message with overlong body lines" do
    let(:long_line_mail) do
      Mail.new.tap do |mail|
        mail.from = 'sender@example.com'
        mail.to = 'rcpt@example.com'
        mail.content_type = 'text/plain'
        mail.body = 'a' * 1500
      end
    end

    it "plans a lossless re-encoding to wrap the lines" do
      plan = plan_for(long_line_mail, [])
      expect(plan.deliverable?).to be true
      item = plan.adjustments.find { |i| i.scope == :body }
      expect(item.encoding).to eq 'quoted-printable'
      expect(item.detail).to match(/998/)

      applied = plan.apply
      longest = applied.encoded.each_line.map { |line| line.chomp.bytesize }.max
      expect(longest).to be <= Mail::EncodingPlan::MAX_LINE_LENGTH
    end
  end

  describe "a message with an undecodable transfer encoding" do
    let(:unknown_cte_mail) do
      Mail.new("Content-Transfer-Encoding: x-strange\r\n\r\n".b + "héllo".b)
    end

    it "blocks when the body cannot be losslessly re-encoded" do
      plan = plan_for(unknown_cte_mail, %w[8BITMIME])
      expect(plan.deliverable?).to be false
      blocker = plan.blockers.find { |i| i.scope == :body }
      expect(blocker.detail).to match(/x-strange/)
    end
  end

  describe "a multipart message" do
    let(:multipart_mail) do
      Mail.new.tap do |mail|
        mail.from = 'sender@example.com'
        mail.to = 'rcpt@example.com'
        mail.subject = 'multipart'
        mail.text_part = Mail::Part.new(content_type: 'text/plain; charset=UTF-8', body: "héllo")
        mail.attachments['data.bin'] = "bin\x00ary".b
      end
    end

    it "analyzes each leaf part separately" do
      plan = plan_for(multipart_mail, [])
      subjects = plan.adjustments.select { |i| i.scope == :body }.map(&:subject)
      expect(subjects).to include(a_string_matching(/text\/plain/))
      expect(subjects).to include(a_string_matching(/data\.bin/))
    end

    it "re-encodes only the parts that need it" do
      plan = plan_for(multipart_mail, %w[8BITMIME])
      expect(plan.deliverable?).to be true

      applied = plan.apply
      text_part = applied.all_parts.find { |p| p.mime_type == 'text/plain' }
      bin_part = applied.all_parts.find { |p| p.mime_type == 'application/octet-stream' }
      expect(text_part.content_transfer_encoding).to be_nil
      expect(bin_part.content_transfer_encoding).to eq 'base64'

      reparsed = Mail.new(applied.encoded)
      expect(reparsed.all_parts.find { |p| p.mime_type == 'application/octet-stream' }.body.decoded)
        .to eq "bin\x00ary".b
    end
  end

  describe "consistency between the plan and the applied result" do
    {
      [] => 'emails/multi_charset/japanese_shift_jis.eml',
      %w[8BITMIME] => 'emails/multi_charset/japanese_shift_jis.eml',
      %w[SMTPUTF8] => 'emails/rfc6532/utf8_headers.eml',
      %w[8BITMIME SMTPUTF8] => 'emails/rfc6532/utf8_headers.eml',
    }.each do |capabilities, fixture|
      it "matches the preflight conclusion for #{capabilities.inspect} with #{File.basename(fixture)}" do
        mail = read_fixture(fixture)
        plan = plan_for(mail, capabilities)
        next unless plan.deliverable?

        applied = plan.apply
        rechecked = plan_for(Mail.new(applied.encoded), capabilities)
        expect(rechecked.deliverable?).to be true
        expect(rechecked.blockers).to be_empty
        expect(rechecked.adjustments).to be_empty
      end
    end

    it "does not modify the original message while planning or applying" do
      mail = read_fixture('emails/rfc6532/utf8_headers.eml')
      before = mail.encoded
      plan_for(mail, %w[SMTPUTF8]).apply
      plan_for(mail, [])
      expect(mail.encoded).to eq before
    end
  end

  describe Mail::Message do
    it "builds a plan for itself" do
      plan = plain_mail.encoding_plan(capabilities: %w[8BITMIME])
      expect(plan).to be_a Mail::EncodingPlan
      expect(plan.message).to be plain_mail
      expect(plan.deliverable?).to be true
    end
  end
end
