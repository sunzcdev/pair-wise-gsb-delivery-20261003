# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

RSpec.describe Mail::EncodingPlan do
  # Fixed capability sets; no network probing involved.
  let(:baseline) { [] } # 7bit SMTP only
  let(:esmtp)    { %i[8bitmime] }
  let(:smtputf8) { %i[smtputf8 8bitmime] }
  let(:full)     { %i[smtputf8 8bitmime binarymime] }

  def plain_message
    Mail.new do
      from    'joe@example.com'
      to      'bob@example.com'
      subject 'Hello'
      body    'Hello there'
    end
  end

  def utf8_message
    Mail.new do
      from         'joe@example.com'
      to           'bob@example.com'
      subject      'Säying Hello'
      content_type 'text/plain; charset=UTF-8'
      body         'Grüße aus München'
    end
  end

  describe 'capabilities' do
    it 'normalizes strings and symbols' do
      plan = Mail::EncodingPlan.new(plain_message, capabilities: ['SMTPUTF8', :'8bitmime', :binarymime])
      expect(plan.capabilities).to eq %i[smtputf8 eight_bit_mime binarymime]
    end

    it 'rejects unknown capabilities' do
      expect do
        Mail::EncodingPlan.new(plain_message, capabilities: %i[starttls])
      end.to raise_error(ArgumentError, /Unknown SMTP capability/)
    end
  end

  describe 'a plain ASCII message' do
    it 'is compatible with every capability set and needs nothing' do
      [baseline, esmtp, smtputf8, full].each do |caps|
        plan = Mail::EncodingPlan.new(plain_message, capabilities: caps)
        expect(plan).to be_compatible
        expect(plan.required_capabilities).to eq []
        expect(plan.reencodings).to be_empty
        expect(plan.blockers).to be_empty
      end
    end

    it 'accepts a plain fixture message as-is' do
      mail = read_fixture('emails', 'plain_emails', 'raw_email.eml')
      plan = mail.encoding_plan
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq []
      expect(plan.blockers).to be_empty
    end
  end

  describe 'non-ASCII header fields' do
    it 'losslessly re-encodes a UTF-8 subject as encoded-words' do
      plan = utf8_message.encoding_plan
      expect(plan).to be_compatible
      subject_reencoding = plan.reencodings.find { |r| r.source == 'header Subject' }
      expect(subject_reencoding).not_to be_nil
      expect(subject_reencoding.to).to eq 'rfc2047'
    end

    it 'applies the plan so the encoded output is 7bit-clean' do
      mail = utf8_message
      encoded = mail.encoding_plan.apply.encoded
      expect(encoded).to be_ascii_only
      expect(encoded).to match(/^Subject: =\?UTF-8\?[BQ]\?/)
      expect(Mail.new(encoded).subject).to eq 'Säying Hello'
    end
  end

  describe 'body transfer encoding' do
    it 're-encodes an 8bit body for a 7bit-only server' do
      plan = utf8_message.encoding_plan
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq []
      body_reencoding = plan.reencodings.find { |r| r.source == 'body' }
      expect(body_reencoding.from).to eq '8bit'
      expect(%w[quoted-printable base64]).to include(body_reencoding.to)
    end

    it 'keeps an 8bit body when the server offers 8BITMIME' do
      plan = utf8_message.encoding_plan(capabilities: esmtp)
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq %i[eight_bit_mime]
      expect(plan.reencodings.map(&:source)).not_to include('body')

      encoded = plan.apply.encoded
      expect(encoded).not_to be_ascii_only
      expect(encoded).to include('Grüße aus München')
    end

    it 're-encodes long lines that exceed the SMTP 998 octet limit' do
      mail = plain_message
      mail.body = 'x' * 2000
      plan = mail.encoding_plan
      expect(plan).to be_compatible
      expect(plan.reencodings.map(&:source)).to include('body')

      encoded = plan.apply.encoded
      expect(encoded.lines.map(&:bytesize).max).to be <= Mail::EncodingPlan::SMTP_LINE_LIMIT
      expect(Mail.new(encoded).body.decoded).to eq 'x' * 2000
    end

    it 're-encodes binary content as base64 for a 7bit-only server' do
      mail = plain_message
      mail.add_part Mail::Part.new(content_type: 'application/octet-stream',
                                   content_transfer_encoding: 'binary',
                                   body: [0, 1, 2, 255, 254].pack('C*') * 50)
      plan = mail.encoding_plan
      expect(plan).to be_compatible
      expect(plan.reencodings.map(&:to)).to include('base64')

      encoded = plan.apply.encoded
      expect(encoded).to be_ascii_only
      expect(Mail.new(encoded).parts.last.body.decoded.b).to eq([0, 1, 2, 255, 254].pack('C*') * 50)
    end

    it 'transports binary content directly when the server offers BINARYMIME' do
      mail = plain_message
      mail.add_part Mail::Part.new(content_type: 'application/octet-stream',
                                   content_transfer_encoding: 'binary',
                                   body: [0, 1, 2, 255, 254].pack('C*') * 50)
      plan = mail.encoding_plan(capabilities: %i[binarymime])
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq %i[binarymime]
      expect(plan.reencodings).to be_empty
    end

    it 'blocks a message/rfc822 part with 8bit content on a 7bit-only server' do
      mail = plain_message
      mail.add_part Mail::Part.new(content_type: 'message/rfc822', body: 'Grüße')
      plan = mail.encoding_plan
      expect(plan).not_to be_compatible
      expect(plan.blockers.map(&:reason)).to include(:no_lossless_encoding)
      expect { plan.apply }.to raise_error(Mail::EncodingPlan::IncompatibleError)
    end

    it 'delivers a message/rfc822 part with 8bit content when 8BITMIME is offered' do
      mail = plain_message
      mail.add_part Mail::Part.new(content_type: 'message/rfc822', body: 'Grüße')
      plan = mail.encoding_plan(capabilities: esmtp)
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq %i[eight_bit_mime]
    end
  end

  describe 'non-ASCII envelope addresses' do
    it 'losslessly re-encodes an internationalized domain with IDNA' do
      mail = plain_message
      mail.smtp_envelope_from = 'joe@münchen.example'
      mail.smtp_envelope_to = ['bob@exämple.net', 'carol@example.org']

      plan = mail.encoding_plan
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq []
      expect(plan.reencodings.map(&:to)).to include('xn--mnchen-3ya.example', 'xn--exmple-cua.net')

      copy = plan.apply
      expect(copy.smtp_envelope_from).to eq 'joe@xn--mnchen-3ya.example'
      expect(copy.smtp_envelope_to).to eq ['bob@xn--exmple-cua.net', 'carol@example.org']

      replanned = copy.encoding_plan
      expect(replanned).to be_compatible
      expect(replanned.reencodings).to be_empty
    end

    it 'encodes well-known IDNA domains correctly' do
      {
        'joe@bücher.example'    => 'joe@xn--bcher-kva.example',
        'joe@例え.jp'           => 'joe@xn--r8jz45g.jp',
        'joe@日本語.jp'          => 'joe@xn--wgv71a119e.jp',
        'joe@münchen.example'   => 'joe@xn--mnchen-3ya.example'
      }.each do |address, expected|
        mail = plain_message
        mail.smtp_envelope_from = address
        expect(mail.encoding_plan.apply.smtp_envelope_from).to eq expected
      end
    end

    it 'never rewrites an internationalized local part into another address' do
      mail = plain_message
      mail.smtp_envelope_from = 'jöe@example.com'

      plan = mail.encoding_plan
      expect(plan).not_to be_compatible
      blocker = plan.blockers.find { |b| b.source == :envelope_from }
      expect(blocker.reason).to eq :internationalized_local_part
      expect(blocker.detail).to include('SMTPUTF8')

      expect { plan.apply }.to raise_error(Mail::EncodingPlan::IncompatibleError) do |error|
        expect(error.blockers).to eq plan.blockers
      end
      expect(mail.smtp_envelope_from).to eq 'jöe@example.com'
    end

    it 'accepts internationalized local parts when the server offers SMTPUTF8' do
      mail = plain_message
      mail.smtp_envelope_from = 'jöe@example.com'
      plan = mail.encoding_plan(capabilities: smtputf8)
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq %i[smtputf8]
      expect(plan.apply.smtp_envelope_from).to eq 'jöe@example.com'
    end
  end

  describe 'internationalized messages (RFC 6531/6532 fixture)' do
    let(:mail) { read_fixture('emails', 'rfc6532', 'utf8_headers.eml') }

    it 'reports concrete blockers on a 7bit-only server' do
      plan = mail.encoding_plan
      expect(plan).not_to be_compatible
      expect(plan.missing_capabilities).to eq %i[smtputf8]

      reasons = plan.blockers.map(&:reason).uniq
      expect(reasons).to include(:internationalized_local_part)
      expect(plan.blockers.map(&:source)).to include(:envelope_from, :envelope_to, 'header From', 'header To')
    end

    it 'still encodes display names and the subject losslessly' do
      plan = mail.encoding_plan
      header_reencodings = plan.reencodings.select { |r| r.source.to_s.start_with?('header') }
      expect(header_reencodings.map(&:source)).to include('header From', 'header To', 'header Subject')
    end

    it 'is deliverable when the server offers SMTPUTF8' do
      plan = mail.encoding_plan(capabilities: smtputf8)
      expect(plan).to be_compatible
      expect(plan.required_capabilities).to eq %i[smtputf8]

      copy = plan.apply
      expect(copy.encoding_plan(capabilities: smtputf8)).to be_compatible
    end
  end

  describe 'multipart messages' do
    def multipart_message
      Mail.new do
        from    'joe@example.com'
        to      'bob@example.com'
        subject 'parts'
        text_part { body 'Grüße text' }
        html_part do
          content_type 'text/html; charset=UTF-8'
          body "<p>#{'x' * 2000}</p>"
        end
      end
    end

    it 'analyzes each part separately' do
      plan = multipart_message.encoding_plan
      expect(plan).to be_compatible
      expect(plan.reencodings.map(&:source)).to include('body part 1', 'body part 2')
    end

    it 're-encodes every part to fit a 7bit-only server' do
      encoded = multipart_message.encoding_plan.apply.encoded
      expect(encoded).to be_ascii_only
      expect(encoded.lines.map(&:bytesize).max).to be <= Mail::EncodingPlan::SMTP_LINE_LIMIT

      roundtripped = Mail.new(encoded)
      expect(roundtripped.parts[0].body.decoded.b).to include('Grüße text'.b)
      expect(roundtripped.parts[1].body.decoded.b).to include('x' * 2000)
    end
  end

  describe 'applying a plan' do
    it 'leaves the original message unchanged' do
      mail = utf8_message
      mail.smtp_envelope_from = 'joe@münchen.example'
      plan = mail.encoding_plan

      expect { plan.apply }.not_to change { mail.encoded }
      expect(mail.smtp_envelope_from).to eq 'joe@münchen.example'
    end

    it 'leaves the parts of a parsed multipart message unchanged' do
      mail = read_fixture('emails', 'mime_emails', 'raw_email7.eml')
      plan = mail.encoding_plan(capabilities: esmtp)
      expect { plan.apply.encoded }.not_to change { mail.parts.map(&:content_transfer_encoding) }
    end

    it 'produces a message whose re-planning matches the preflight conclusion' do
      mail = utf8_message
      mail.smtp_envelope_from = 'joe@münchen.example'

      [baseline, esmtp].each do |caps|
        plan = mail.encoding_plan(capabilities: caps)
        expect(plan).to be_compatible
        replanned = Mail::EncodingPlan.new(plan.apply, capabilities: caps)
        expect(replanned).to be_compatible
        expect(replanned.blockers).to be_empty
        expect(replanned.missing_capabilities).to be_empty
      end
    end
  end
end
