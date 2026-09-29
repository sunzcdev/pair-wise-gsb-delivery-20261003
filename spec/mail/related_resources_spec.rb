# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

RSpec.describe Mail::RelatedResources do

  describe "with nested multipart/related containers sharing a Content-ID" do
    let(:mail) { read_fixture('emails', 'mime_emails', 'raw_email_with_nested_related_shared_cid.eml') }
    let(:view) { mail.related_resources }

    def inner_related_part
      mail.all_parts.detect { |p| p.multipart? && p.boundary == 'rel-inner' }
    end

    it "finds the outermost multipart/related container" do
      expect(view).to be_a Mail::RelatedResources
      expect(view.container.boundary).to eq 'rel-outer'
    end

    it "selects the root body part with the start parameter, not the first part" do
      expect(view.root.mime_type).to eq 'text/html'
      expect(view.root.content_id).to eq '<outer-body@example>'
    end

    it "resolves a shared Content-ID within the enclosing scope only" do
      expect(view['shared@example'].decoded).to eq 'OUTER-IMAGE'
    end

    it "accepts bracketed and percent-escaped Content-IDs" do
      expect(view['<shared@example>'].decoded).to eq 'OUTER-IMAGE'
      expect(view['shared%40example'].decoded).to eq 'OUTER-IMAGE'
    end

    it "collects cid: references from the root body and ignores external URLs" do
      expect(view.references).to eq %w(shared@example outer-only@example missing@example)
    end

    it "lists the resources referenced by the root body" do
      expect(view.referenced.map(&:decoded)).to match_array %w(OUTER-IMAGE OUTER-ONLY)
    end

    it "lists resources nothing refers to" do
      expect(view.unreferenced.map(&:decoded)).to eq %w(UNUSED-GIF)
    end

    it "lists dangling references with no matching resource" do
      expect(view.dangling).to eq %w(missing@example)
    end

    it "does not treat the same ID in a nested related container as a conflict" do
      expect(view.ambiguous).to be_empty
      expect(view.resources.map(&:decoded)).to match_array %w(OUTER-IMAGE OUTER-ONLY UNUSED-GIF)
    end

    describe "the nested container's own view" do
      let(:inner_view) { inner_related_part.related_resources }

      it "is scoped to the nested container" do
        expect(inner_view.container.boundary).to eq 'rel-inner'
      end

      it "selects the nested root body part with its own start parameter" do
        expect(inner_view.root.content_id).to eq '<inner-body@example>'
      end

      it "resolves the shared Content-ID to the nested resource, not the outer one" do
        expect(inner_view['shared@example'].decoded).to eq 'INNER-IMAGE'
        expect(inner_view.referenced.map(&:decoded)).to eq %w(INNER-IMAGE)
        expect(inner_view.unreferenced).to be_empty
        expect(inner_view.dangling).to be_empty
        expect(inner_view.ambiguous).to be_empty
      end
    end
  end

  describe "root selection without a usable start parameter" do
    it "falls back to the first part when start is missing" do
      mail = Mail.new("Content-Type: multipart/related; boundary=b\r\n\r\n" \
                      "--b\r\nContent-Type: text/html\r\n\r\n<p>hi</p>\r\n--b--\r\n")
      expect(mail.related_resources.root.mime_type).to eq 'text/html'
    end

    it "falls back to the first part when start does not resolve" do
      mail = Mail.new("Content-Type: multipart/related; boundary=b; start=<no-such@example>\r\n\r\n" \
                      "--b\r\nContent-Type: text/html\r\n\r\n<p>hi</p>\r\n--b--\r\n")
      expect(mail.related_resources.root.mime_type).to eq 'text/html'
    end
  end

  describe "duplicate Content-IDs within a single scope" do
    let(:mail) do
      Mail.new("Content-Type: multipart/related; boundary=b; start=<root@example>\r\n" \
               "\r\n" \
               "--b\r\n" \
               "Content-Type: text/html\r\n" \
               "Content-ID: <root@example>\r\n" \
               "\r\n" \
               "<img src=\"cid:dup@example\">\r\n" \
               "--b\r\n" \
               "Content-Type: image/png\r\n" \
               "Content-ID: <dup@example>\r\n" \
               "\r\n" \
               "AAA\r\n" \
               "--b\r\n" \
               "Content-Type: image/png\r\n" \
               "Content-ID: <dup@example>\r\n" \
               "\r\n" \
               "BBB\r\n" \
               "--b--\r\n")
    end
    let(:view) { mail.related_resources }

    it "reports the clash as ambiguous" do
      expect(view.ambiguous.keys).to eq %w(dup@example)
      expect(view.ambiguous['dup@example'].map(&:decoded)).to eq %w(AAA BBB)
      expect(view.ambiguous?('dup@example')).to be true
      expect(view.ambiguous?('root@example')).to be false
    end

    it "still resolves to the first matching part" do
      expect(view['dup@example'].decoded).to eq 'AAA'
      expect(view.referenced.map(&:decoded)).to eq %w(AAA BBB)
      expect(view.dangling).to be_empty
    end
  end

  describe "messages without multipart/related" do
    it "returns nil" do
      mail = Mail.new("Content-Type: text/plain\r\n\r\nhello\r\n")
      expect(mail.related_resources).to be_nil
    end
  end
end
