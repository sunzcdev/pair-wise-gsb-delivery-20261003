# frozen_string_literal: true

# Copyright (C) Bob Aman
#
#    Licensed under the Apache License, Version 2.0 (the "License");
#    you may not use this file except in compliance with the License.
#    You may obtain a copy of the License at
#
#        http://www.apache.org/licenses/LICENSE-2.0
#
#    Unless required by applicable law or agreed to in writing, software
#    distributed under the License is distributed on an "AS IS" BASIS,
#    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#    See the License for the specific language governing permissions and
#    limitations under the License.

require "spec_helper"

require "addressable/mailto"

describe Addressable::MailTo, "when parsing a simple mailto URI" do
  subject(:mail) { described_class.parse("mailto:user@example.com") }

  it "has a single recipient" do
    expect(mail.to).to eq(["user@example.com"])
  end

  it "has no headers" do
    expect(mail.headers).to eq([])
  end

  it "round-trips to the same URI" do
    expect(mail.to_s).to eq("mailto:user@example.com")
  end
end

describe Addressable::MailTo, "when parsing multiple recipients" do
  subject(:mail) do
    described_class.parse(
      "mailto:first@example.com,second@example.com,third@example.com"
    )
  end

  it "preserves recipient order" do
    expect(mail.to).to eq(
      ["first@example.com", "second@example.com", "third@example.com"]
    )
  end
end

describe Addressable::MailTo, "when parsing a plus-addressed recipient" do
  subject(:mail) do
    described_class.parse("mailto:user+tag@example.com?subject=a+b")
  end

  it "does not treat '+' as a space in the recipient" do
    expect(mail.to).to eq(["user+tag@example.com"])
  end

  it "does not treat '+' as a space in header values" do
    expect(mail.subject).to eq("a+b")
  end
end

describe Addressable::MailTo, "when parsing an encoded comma in a recipient" do
  subject(:mail) do
    described_class.parse("mailto:Doe%2C%20John%20%3Cjohn@example.com%3E")
  end

  it "does not split the recipient at the encoded comma" do
    expect(mail.to).to eq(["Doe, John <john@example.com>"])
  end

  it "re-encodes the comma so it cannot act as a separator" do
    expect(described_class.parse(mail.to_s).to).to eq(
      ["Doe, John <john@example.com>"]
    )
  end
end

describe Addressable::MailTo, "when parsing duplicate header fields" do
  subject(:mail) do
    described_class.parse(
      "mailto:user@example.com?x-tag=one&subject=Hi&x-tag=two"
    )
  end

  it "preserves duplicates and their order" do
    expect(mail.headers).to eq(
      [["x-tag", "one"], ["subject", "Hi"], ["x-tag", "two"]]
    )
  end

  it "returns all values for a repeated field" do
    expect(mail.header_values("X-Tag")).to eq(["one", "two"])
  end

  it "returns the first value for a repeated field" do
    expect(mail["x-tag"]).to eq("one")
  end
end

describe Addressable::MailTo, "when parsing Unicode and multiline content" do
  subject(:mail) do
    described_class.parse(
      "mailto:user@example.com" \
      "?subject=%E4%B8%AD%E6%96%87%E4%B8%BB%E9%A2%98" \
      "&body=%E7%AC%AC%E4%B8%80%E8%A1%8C%0D%0A%E7%AC%AC%E4%BA%8C%E8%A1%8C"
    )
  end

  it "decodes the UTF-8 subject" do
    expect(mail.subject).to eq("中文主题")
  end

  it "decodes CRLF line breaks in the body" do
    expect(mail.body).to eq("第一行\r\n第二行")
  end
end

describe Addressable::MailTo, "when parsing an invalid URI" do
  it "rejects a non-mailto URI" do
    expect { described_class.parse("http://example.com/") }.to raise_error(
      Addressable::MailTo::InvalidMailToError
    )
  end

  it "rejects a URI with a fragment" do
    expect { described_class.parse("mailto:a@b#frag") }.to raise_error(
      Addressable::MailTo::InvalidMailToError
    )
  end

  it "rejects unencoded spaces" do
    expect { described_class.parse("mailto:a b@example.com") }.to raise_error(
      Addressable::MailTo::InvalidMailToError
    )
  end

  it "rejects malformed percent-encoding" do
    expect { described_class.parse("mailto:a%2zb@example.com") }.to raise_error(
      Addressable::MailTo::InvalidMailToError
    )
  end

  it "rejects an empty recipient between commas" do
    expect { described_class.parse("mailto:a@b,,c@d") }.to raise_error(
      Addressable::MailTo::InvalidMailToError
    )
  end

  it "rejects a header field without '='" do
    expect { described_class.parse("mailto:a@b?subject") }.to raise_error(
      Addressable::MailTo::InvalidMailToError
    )
  end

  it "rejects an invalid header field name" do
    expect { described_class.parse("mailto:a@b?bad%20name=x") }.to raise_error(
      Addressable::MailTo::InvalidHeaderNameError
    )
  end

  it "rejects a non-string input" do
    expect { described_class.parse(42) }.to raise_error(TypeError)
  end
end

describe Addressable::MailTo, "when constructed from parts" do
  subject(:mail) do
    described_class.new(
      to: ["user+tag@example.com", "other@example.com"],
      headers: [["subject", "中文主题"], ["body", "第一行\n第二行"]]
    )
  end

  it "keeps the plus address literal" do
    expect(mail.to_s).to include("user+tag@example.com")
  end

  it "percent-encodes the Unicode subject as UTF-8" do
    expect(mail.to_s).to include(
      "subject=%E4%B8%AD%E6%96%87%E4%B8%BB%E9%A2%98"
    )
  end

  it "encodes body line breaks as %0D%0A" do
    expect(mail.to_s).to include(
      "body=%E7%AC%AC%E4%B8%80%E8%A1%8C%0D%0A%E7%AC%AC%E4%BA%8C%E8%A1%8C"
    )
  end

  it "round-trips through parse" do
    parsed = described_class.parse(mail.to_s)
    expect(parsed.to).to eq(["user+tag@example.com", "other@example.com"])
    expect(parsed.subject).to eq("中文主题")
    expect(parsed.body).to eq("第一行\r\n第二行")
  end

  it "accepts a single recipient string" do
    expect(described_class.new(to: "a@b").to).to eq(["a@b"])
  end

  it "accepts a Hash of headers" do
    mail = described_class.new(headers: { "subject" => "Hi" })
    expect(mail.subject).to eq("Hi")
  end

  it "expands Array values in a Hash of headers into repeated fields" do
    mail = described_class.new(headers: { "x-tag" => ["one", "two"] })
    expect(mail.header_values("x-tag")).to eq(["one", "two"])
  end

  it "produces an Addressable::URI" do
    uri = mail.to_uri
    expect(uri).to be_an(Addressable::URI)
    expect(uri.scheme).to eq("mailto")
  end

  it "transcodes non-UTF-8 strings before percent-encoding" do
    mail = described_class.new(
      headers: [["subject", "中文主题".encode(Encoding::GBK)]]
    )
    expect(mail.to_s).to include(
      "subject=%E4%B8%AD%E6%96%87%E4%B8%BB%E9%A2%98"
    )
    expect(described_class.parse(mail.to_s).subject).to eq("中文主题")
  end
end

describe Addressable::MailTo, "when constructed with invalid input" do
  it "rejects an empty header field name" do
    expect {
      described_class.new(headers: [["", "value"]])
    }.to raise_error(Addressable::MailTo::InvalidHeaderNameError)
  end

  it "rejects a header field name containing a space" do
    expect {
      described_class.new(headers: [["bad name", "value"]])
    }.to raise_error(Addressable::MailTo::InvalidHeaderNameError)
  end

  it "rejects a header field name containing '='" do
    expect {
      described_class.new(headers: [["bad=name", "value"]])
    }.to raise_error(Addressable::MailTo::InvalidHeaderNameError)
  end

  it "rejects a header field name containing non-ASCII characters" do
    expect {
      described_class.new(headers: [["主题", "value"]])
    }.to raise_error(Addressable::MailTo::InvalidHeaderNameError)
  end

  it "rejects an empty recipient" do
    expect {
      described_class.new(to: [""])
    }.to raise_error(Addressable::MailTo::InvalidMailToError)
  end

  it "rejects a non-string recipient" do
    expect {
      described_class.new(to: [nil])
    }.to raise_error(TypeError)
  end

  it "rejects a non-string header value" do
    expect {
      described_class.new(headers: [["subject", nil]])
    }.to raise_error(TypeError)
  end

  it "rejects a string that cannot be transcoded to UTF-8" do
    expect {
      described_class.new(
        headers: [["subject", "\xD6".dup.force_encoding(Encoding::GBK)]]
      )
    }.to raise_error(Addressable::MailTo::InvalidMailToError)
  end
end

describe Addressable::MailTo, "when modifying the subject of a parsed URI" do
  subject(:mail) do
    described_class.parse(
      "mailto:user+tag@example.com,other@example.com" \
      "?subject=%E4%B8%AD%E6%96%87%E4%B8%BB%E9%A2%98" \
      "&body=%E7%AC%AC%E4%B8%80%E8%A1%8C%0D%0A%E7%AC%AC%E4%BA%8C%E8%A1%8C" \
      "&x-tag=one&x-tag=two"
    )
  end

  it "leaves every other field intact" do
    mail.subject = "新主题"
    reparsed = described_class.parse(mail.to_s)
    expect(reparsed.to).to eq(["user+tag@example.com", "other@example.com"])
    expect(reparsed.subject).to eq("新主题")
    expect(reparsed.body).to eq("第一行\r\n第二行")
    expect(reparsed.header_values("x-tag")).to eq(["one", "two"])
  end

  it "replaces all existing subject fields" do
    mail = described_class.parse("mailto:a@b?subject=one&subject=two")
    mail.subject = "three"
    expect(mail.header_values("subject")).to eq(["three"])
  end

  it "does not disturb the original when working on a dup" do
    copy = mail.dup
    copy.subject = "changed"
    copy.add_header("x-tag", "three")
    expect(mail.subject).to eq("中文主题")
    expect(mail.header_values("x-tag")).to eq(["one", "two"])
  end
end

describe Addressable::MailTo, "round-trip stability" do
  it "parses its own output back to an equal object" do
    mail = described_class.new(
      to: ["user+tag@example.com", "Doe, John <john@example.com>"],
      headers: [
        ["subject", "中文主题"],
        ["body", "第一行\n第二行\n\n第四行"],
        ["x-tag", "one"],
        ["x-tag", "two"]
      ]
    )
    expect(described_class.parse(mail.to_s)).to eq(
      described_class.new(
        to: ["user+tag@example.com", "Doe, John <john@example.com>"],
        headers: [
          ["subject", "中文主题"],
          ["body", "第一行\r\n第二行\r\n\r\n第四行"],
          ["x-tag", "one"],
          ["x-tag", "two"]
        ]
      )
    )
  end

  it "produces a stable URI when parsed and re-serialized" do
    uri = "mailto:user+tag@example.com,other@example.com" \
      "?subject=%E4%B8%AD%E6%96%87%E4%B8%BB%E9%A2%98" \
      "&body=line1%0D%0Aline2"
    expect(described_class.parse(uri).to_s).to eq(uri)
  end
end
