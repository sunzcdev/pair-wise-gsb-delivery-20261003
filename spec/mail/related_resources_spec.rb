# encoding: utf-8
# frozen_string_literal: true
require 'spec_helper'

RSpec.describe Mail::RelatedResources do
  let(:mail) { read_fixture('emails', 'mime_emails', 'raw_email_nested_related.eml') }
  let(:view) { mail.related_resources }
  let(:inner_view) { view.nested.first }

  def cids(parts)
    parts.map { |part| Mail::Utilities.unbracket(part.content_id) }
  end

  describe "message integration" do
    it "detects multipart/related messages" do
      expect(mail.multipart_related?).to be true
      expect(mail.related_resources).to be_a(Mail::RelatedResources)
    end

    it "returns nil for messages that are not multipart/related" do
      plain = Mail.new(content_type: 'text/plain', body: 'hello')
      expect(plain.multipart_related?).to be false
      expect(plain.related_resources).to be_nil
    end

    it "exposes the view on nested parts too" do
      inner = mail.parts.find { |part| part.multipart_related? }
      expect(inner.related_resources).to be_a(Mail::RelatedResources)
    end
  end

  describe "root body selection" do
    it "uses the start parameter to find the root body" do
      expect(view.start).to eq 'outer-body@example'
      expect(view.root.mime_type).to eq 'text/html'
      expect(Mail::Utilities.unbracket(view.root.content_id)).to eq 'outer-body@example'
    end

    it "does not default to the first part when start points elsewhere" do
      expect(view.root).not_to eq mail.parts.first
    end

    it "falls back to the first part when there is no start parameter" do
      expect(inner_view.start).to be_nil
      expect(inner_view.root.mime_type).to eq 'text/html'
      expect(Mail::Utilities.unbracket(inner_view.root.content_id)).to eq 'inner-body@example'
    end

    it "returns a nil root when start names a missing part" do
      message = Mail.new
      message.content_type = 'multipart/related; boundary=x; start="<nope@example>"'
      message.part content_type: 'text/html', content_id: '<body@example>', body: '<html></html>'
      expect(message.related_resources.root).to be_nil
    end
  end

  describe "reference extraction" do
    it "lists the cids referenced by the root body" do
      expect(view.references).to eq %w(shared@example outer-only@example missing@example)
    end

    it "extracts references from the nested root body" do
      expect(inner_view.references).to eq %w(shared@example)
    end
  end

  describe "resource scoping" do
    it "lists only resources belonging to its own scope" do
      expect(cids(view.resources)).to match_array %w(shared@example outer-only@example unreferenced@example)
      expect(cids(inner_view.resources)).to eq %w(shared@example)
    end

    it "resolves a shared Content-ID to the resource of the same scope" do
      expect(view.resolve('shared@example').decoded).to include 'OUTER-SHARED-IMAGE'
      expect(inner_view.resolve('shared@example').decoded).to include 'INNER-SHARED-IMAGE'
    end

    it "does not let nested resources leak into the outer scope" do
      expect(view.resources.map(&:decoded).join).not_to include 'INNER-SHARED-IMAGE'
    end

    it "does not let outer resources leak into the nested scope" do
      expect(inner_view.resources.map(&:decoded).join).not_to include 'OUTER-SHARED-IMAGE'
      expect(inner_view.resolve('outer-only@example')).to be_nil
    end

    it "accepts bracketed and cid:-prefixed forms when resolving" do
      expect(view.resolve('<shared@example>')).to eq view.resolve('cid:shared@example')
    end
  end

  describe "referenced, unreferenced and dangling resources" do
    it "lists resources referenced by the root body" do
      expect(cids(view.referenced)).to match_array %w(shared@example outer-only@example)
    end

    it "lists resources never referenced by the root body" do
      expect(cids(view.unreferenced)).to eq %w(unreferenced@example)
    end

    it "lists references with no matching resource in scope" do
      expect(view.dangling_references).to eq %w(missing@example)
    end

    it "reports no dangling references for the self-contained nested scope" do
      expect(inner_view.dangling_references).to be_empty
      expect(inner_view.unreferenced).to be_empty
      expect(cids(inner_view.referenced)).to eq %w(shared@example)
    end
  end

  describe "nested scopes" do
    it "exposes nested multipart/related containers as their own views" do
      expect(view.nested.size).to eq 1
      expect(view.nested.first).to be_a(Mail::RelatedResources)
      expect(inner_view.nested).to be_empty
    end

    it "treats the same Content-ID in different scopes as distinct resources" do
      expect(view.ambiguous).to be_empty
      expect(inner_view.ambiguous).to be_empty
      expect(view.resolve('shared@example')).not_to eq inner_view.resolve('shared@example')
    end
  end

  describe "ambiguity within one scope" do
    let(:ambiguous_mail) do
      Mail.new.tap do |message|
        message.content_type = 'multipart/related; boundary=amb; start="<body@example>"'
        message.part content_type: 'text/html', content_id: '<body@example>',
                     body: '<html><body><img src="cid:dup@example"></body></html>'
        message.part content_type: 'image/png', content_id: '<dup@example>', body: 'FIRST'
        message.part content_type: 'image/png', content_id: '<dup@example>', body: 'SECOND'
      end
    end

    it "reports duplicate Content-IDs in the same scope as ambiguous" do
      ambiguous = ambiguous_mail.related_resources.ambiguous
      expect(ambiguous.keys).to eq %w(dup@example)
      expect(ambiguous['dup@example'].size).to eq 2
    end

    it "raises AmbiguousResourceError when resolving an ambiguous Content-ID" do
      expect {
        ambiguous_mail.related_resources.resolve('dup@example')
      }.to raise_error(Mail::RelatedResources::AmbiguousResourceError, /dup@example/)
    end
  end
end
