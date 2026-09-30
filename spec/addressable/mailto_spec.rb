# frozen_string_literal: true

require "spec_helper"

require "addressable"

describe Addressable::Mailto do
  describe ".build / #to_s" do
    it "constructs a single-recipient mailto URI" do
      mailto = Addressable::Mailto.build(:to => "a@example.com")
      expect(mailto.to_s).to eq("mailto:a@example.com")
    end

    it "preserves recipient order for multiple recipients" do
      mailto = Addressable::Mailto.build(
        :to => ["a@example.com", "b@example.com", "c@example.com"]
      )
      expect(mailto.to_s).to eq(
        "mailto:a@example.com,b@example.com,c@example.com"
      )
    end

    it "preserves repeated header fields" do
      mailto = Addressable::Mailto.build(
        :headers => [
          ["x-custom", "one"],
          ["x-custom", "two"]
        ]
      )
      expect(mailto.to_s).to eq("mailto:?x-custom=one&x-custom=two")
      expect(mailto.headers).to eq(
        [["x-custom", "one"], ["x-custom", "two"]]
      )
    end

    it "accepts headers as a hash" do
      mailto = Addressable::Mailto.build(
        :to => "a@example.com",
        :headers => {"subject" => "hi"}
      )
      expect(mailto.to_s).to eq("mailto:a@example.com?subject=hi")
    end

    it "does not encode a plus sign in an address" do
      mailto = Addressable::Mailto.build(:to => "user+tag@example.com")
      expect(mailto.to_s).to eq("mailto:user+tag@example.com")
    end

    it "percent-encodes a literal comma within an address" do
      mailto = Addressable::Mailto.build(:to => "a,b@example.com")
      expect(mailto.to_s).to eq("mailto:a%2Cb@example.com")
    end

    it "percent-encodes a plus sign in a field value as itself, not a space" do
      mailto = Addressable::Mailto.build(
        :headers => [["subject", "a+b"]]
      )
      expect(mailto.to_s).to eq("mailto:?subject=a+b")
    end

    it "percent-encodes spaces as %20, never a plus sign" do
      mailto = Addressable::Mailto.build(
        :headers => [["subject", "a b"]]
      )
      expect(mailto.to_s).to eq("mailto:?subject=a%20b")
    end

    it "percent-encodes Unicode in a subject" do
      mailto = Addressable::Mailto.build(
        :headers => [["subject", "通知"]]
      )
      expect(mailto.to_s).to eq("mailto:?subject=%E9%80%9A%E7%9F%A5")
    end

    it "percent-encodes newlines in the body as CRLF" do
      mailto = Addressable::Mailto.build(
        :headers => [["body", "one\ntwo\rthree\r\nfour"]]
      )
      expect(mailto.to_s).to eq(
        "mailto:?body=one%0D%0Atwo%0D%0Athree%0D%0Afour"
      )
    end

    it "percent-encodes an ampersand in a field value" do
      mailto = Addressable::Mailto.build(
        :headers => [["body", "a&b=c"]]
      )
      # The ampersand delimits fields and must be encoded; the first
      # equals sign already delimits the name, so a literal '=' in the
      # value is allowed by RFC 6068.
      expect(mailto.to_s).to eq("mailto:?body=a%26b=c")
    end

    it "percent-encodes structural characters in a field name and round-trips" do
      # RFC 5322 ftext allows characters such as '=', but inside a mailto
      # query they must be escaped so they do not delimit fields.
      mailto = Addressable::Mailto.build(
        :headers => [["a=b", "v"]]
      )
      expect(mailto.to_s).to eq("mailto:?a%3Db=v")
      reparsed = Addressable::Mailto.parse(mailto.to_s)
      expect(reparsed.headers).to eq([["a=b", "v"]])
    end

    it "raises for an invalid field name" do
      expect do
        Addressable::Mailto.build(:headers => [["bad name", "x"]])
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)

      expect do
        Addressable::Mailto.build(:headers => [["bad:name", "x"]])
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)

      expect do
        Addressable::Mailto.build(:headers => [["", "x"]])
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)
    end

    it "raises for a non-string value that cannot be converted" do
      expect do
        Addressable::Mailto.build(:to => Object.new)
      end.to raise_error(TypeError)
    end

    it "rejects a headers option of the wrong type" do
      expect do
        Addressable::Mailto.build(:headers => "nope")
      end.to raise_error(TypeError)
    end
  end

  describe ".parse" do
    it "parses a single recipient from the path" do
      mailto = Addressable::Mailto.parse("mailto:a@example.com")
      expect(mailto.to).to eq(["a@example.com"])
    end

    it "parses multiple recipients in order" do
      mailto = Addressable::Mailto.parse(
        "mailto:a@example.com,b@example.com,c@example.com"
      )
      expect(mailto.to).to eq(
        ["a@example.com", "b@example.com", "c@example.com"]
      )
    end

    it "does not treat a plus sign as a space" do
      mailto = Addressable::Mailto.parse("mailto:user+tag@example.com")
      expect(mailto.to).to eq(["user+tag@example.com"])
    end

    it "does not split an encoded comma into a second recipient" do
      mailto = Addressable::Mailto.parse("mailto:a%2Cb@example.com,b@x.com")
      expect(mailto.to).to eq(["a,b@example.com", "b@x.com"])
    end

    it "does not treat a plus in a field value as a space" do
      mailto = Addressable::Mailto.parse("mailto:?subject=a+b")
      expect(mailto.subject).to eq("a+b")
    end

    it "decodes Unicode in a subject" do
      mailto = Addressable::Mailto.parse(
        "mailto:?subject=%E9%80%9A%E7%9F%A5"
      )
      expect(mailto.subject).to eq("通知")
    end

    it "decodes a multi-line body" do
      mailto = Addressable::Mailto.parse("mailto:?body=a%0D%0Ab%0Ac")
      expect(mailto.body).to eq("a\r\nb\nc")
    end

    it "preserves repeated header fields" do
      mailto = Addressable::Mailto.parse("mailto:?x=one&x=two&subject=hi")
      expect(mailto.headers).to eq(
        [["x", "one"], ["x", "two"], ["subject", "hi"]]
      )
    end

    it "reads recipients from a to header field as well as the path" do
      mailto = Addressable::Mailto.parse(
        "mailto:a@example.com?to=b%2Ctag@example.com&subject=hi"
      )
      expect(mailto.to).to eq(["a@example.com", "b,tag@example.com"])
      expect(mailto.subject).to eq("hi")
    end

    it "matches header field names case-insensitively" do
      mailto = Addressable::Mailto.parse("mailto:?SUBJECT=hi")
      expect(mailto.subject).to eq("hi")
    end

    it "returns an empty recipient list for a bare mailto" do
      mailto = Addressable::Mailto.parse("mailto:")
      expect(mailto.to).to eq([])
      expect(mailto.headers).to eq([])
    end

    it "raises for a non-mailto URI" do
      expect do
        Addressable::Mailto.parse("http://example.com")
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)
    end

    it "raises for a fragment" do
      expect do
        Addressable::Mailto.parse("mailto:a@example.com#frag")
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)
    end

    it "raises for a stray percent sign" do
      expect do
        Addressable::Mailto.parse("mailto:abc%")
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)
    end

    it "raises for invalid UTF-8 bytes" do
      expect do
        Addressable::Mailto.parse("mailto:?subject=%FF")
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)
    end

    it "raises for an invalid field name" do
      expect do
        Addressable::Mailto.parse("mailto:?bad%20name=x")
      end.to raise_error(Addressable::Mailto::InvalidMailtoError)
    end
  end

  describe "round-trip" do
    let(:original) do
      "mailto:user+tag@example.com,a%2Cb@example.com" \
        "?subject=%E9%80%9A%E7%9F%A5" \
        "&body=%E7%AC%AC%E4%B8%80%E8%A1%8C%0D%0Asecond+line%0D%0A" \
        "&cc=list%40example.com"
    end

    it "preserves recipients, subject, and body across a parse/build cycle" do
      mailto = Addressable::Mailto.parse(original)

      expect(mailto.to).to eq(["user+tag@example.com", "a,b@example.com"])
      expect(mailto.subject).to eq("通知")
      expect(mailto.body).to eq("第一行\r\nsecond+line\r\n")
      expect(mailto.header("cc")).to eq("list@example.com")

      rebuilt = Addressable::Mailto.parse(mailto.to_s)
      expect(rebuilt.to).to eq(mailto.to)
      expect(rebuilt.subject).to eq(mailto.subject)
      expect(rebuilt.body).to eq(mailto.body)
    end

    it "does not rewrite other fields when the subject changes" do
      mailto = Addressable::Mailto.parse(original)
      recipients_before = mailto.to.dup
      body_before = mailto.body
      cc_before = mailto.header("cc")

      mailto.subject = "新主题"

      expect(mailto.subject).to eq("新主题")
      expect(mailto.to).to eq(recipients_before)
      expect(mailto.body).to eq(body_before)
      expect(mailto.header("cc")).to eq(cc_before)

      reparsed = Addressable::Mailto.parse(mailto.to_s)
      expect(reparsed.to).to eq(recipients_before)
      expect(reparsed.body).to eq(body_before)
      expect(reparsed.header("cc")).to eq(cc_before)
      expect(reparsed.subject).to eq("新主题")
    end
  end

  describe "mutation" do
    it "appends recipients and headers" do
      mailto = Addressable::Mailto.new
      mailto.add_recipient("a@example.com")
      mailto.add_recipients(["b@example.com", "c@example.com"])
      mailto.add_header("subject", "hi")
      expect(mailto.to).to eq(
        ["a@example.com", "b@example.com", "c@example.com"]
      )
      expect(mailto.subject).to eq("hi")
    end

    it "returns nil for a missing header" do
      mailto = Addressable::Mailto.new
      expect(mailto.header("subject")).to be_nil
      expect(mailto.subject).to be_nil
    end

    it "removes the subject when assigned nil" do
      mailto = Addressable::Mailto.parse("mailto:?subject=hi&body=b")
      mailto.subject = nil
      expect(mailto.subject).to be_nil
      expect(mailto.body).to eq("b")
    end

    it "produces an independent dup" do
      mailto = Addressable::Mailto.parse("mailto:a@example.com?subject=hi")
      copy = mailto.dup
      copy.subject = "changed"
      expect(mailto.subject).to eq("hi")
    end

    it "compares equivalent mailto URIs" do
      a = Addressable::Mailto.parse("mailto:?subject=hi")
      b = Addressable::Mailto.build(:headers => [["subject", "hi"]])
      expect(a).to eq(b)
    end
  end

  describe "Addressable::URI integration" do
    it "constructs a URI via Addressable::URI.mailto" do
      uri = Addressable::URI.mailto(
        :to => ["user+tag@example.com", "a,b@example.com"],
        :headers => [["subject", "通知"]]
      )
      expect(uri).to be_a(Addressable::URI)
      expect(uri.scheme).to eq("mailto")
      expect(uri.mailto.to).to eq(
        ["user+tag@example.com", "a,b@example.com"]
      )
      expect(uri.mailto.subject).to eq("通知")
    end

    it "exposes a structured view via #mailto" do
      uri = Addressable::URI.parse(
        "mailto:user+tag@example.com?body=line1%0D%0Aline2"
      )
      view = uri.mailto
      expect(view).to be_a(Addressable::Mailto)
      expect(view.to).to eq(["user+tag@example.com"])
      expect(view.body).to eq("line1\r\nline2")
    end

    it "round-trips through Addressable::URI" do
      uri = Addressable::URI.mailto(
        :to => ["user+tag@example.com"],
        :headers => [["subject", "通知"], ["body", "a\nb"]]
      )
      expect(uri.mailto.to).to eq(["user+tag@example.com"])
      expect(uri.mailto.subject).to eq("通知")
      expect(uri.mailto.body).to eq("a\r\nb")
    end
  end
end
