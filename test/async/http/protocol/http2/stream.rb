# frozen_string_literal: true

# Released under the MIT License.
# Copyright, 2026, by Samuel Williams.

require "async/http/protocol/http2/stream"

describe Async::HTTP::Protocol::HTTP2::Stream do
  let(:output) do
    Class.new do
      def close_stream
      end

      def stop(error)
      end
    end.new
  end

  let(:stream) do
    Class.new(subject) do
      def initialize(output)
        @input = nil
        @output = output
        @pool = nil
        @connection = nil
        @headers = nil
        @app_headers = nil
      end
    end.new(output)
  end

  it "closes the output stream on an orderly closure" do
    expect(output).to receive(:close_stream)

    stream.closed(nil)
  end

  it "stops the output stream on an error" do
    error = RuntimeError.new("Stream failed!")
    expect(output).to receive(:stop).with(error)

    stream.closed(error)
  end

  with "#publish_headers" do
    it "returns a stable snapshot which does not share the working fields" do
      headers = Protocol::HTTP::Headers.new([["x-request", "first"]])
      stream.headers = headers

      app_headers = stream.publish_headers(headers)

      expect(stream.app_headers).to be_equal(app_headers)
      expect(app_headers).not.to be_equal(headers)
      expect(app_headers.fields).to be == [["x-request", "first"]]

      # Mutating the protocol-side working copy must not touch the application view.
      headers.add("etag", "buffered", trailer: true)

      expect(app_headers.fields).to be == [["x-request", "first"]]
    end
  end

  with "#receive_trailing_headers" do
    let(:headers) {Protocol::HTTP::Headers.new([["x-request", "first"]])}

    def before
      super

      stream.headers = headers
      stream.publish_headers(headers)
    end

    it "keeps received trailers in a distinct trailer section" do
      stream.receive_trailing_headers([["etag", "abcd"]], true)

      expect(headers).to be(:trailer?)
      expect(headers.header.to_a).to be == [["x-request", "first"]]
      expect(headers.trailer.to_a).to be == [["etag", "abcd"]]
    end

    it "does not create a trailer section for an empty trailing frame" do
      stream.receive_trailing_headers([], true)

      expect(headers).not.to be(:trailer?)
      expect(headers.trailer.to_a).to be(:empty?)
    end

    it "rejects fields which are forbidden in trailers" do
      expect do
        stream.receive_trailing_headers([["host", "example.com"]], true)
      end.to raise_exception(Protocol::HTTP::InvalidTrailerError)
    end
  end

  with "#merge_received_trailers" do
    let(:headers) {Protocol::HTTP::Headers.new([["x-request", "first"]])}

    def before
      super

      stream.headers = headers
      stream.publish_headers(headers)
    end

    it "copies buffered trailers into the application view without touching the initial section" do
      stream.receive_trailing_headers([["etag", "abcd"]], true)
      stream.merge_received_trailers

      app_headers = stream.app_headers

      expect(app_headers).to be(:trailer?)
      expect(app_headers.header.to_a).to be == [["x-request", "first"]]
      expect(app_headers.trailer.to_a).to be == [["etag", "abcd"]]
      # Field-name lookups still see the merged trailer:
      expect(app_headers["etag"]).to be == "abcd"
    end

    it "is a no-op when no trailers were received" do
      stream.merge_received_trailers

      expect(stream.app_headers).not.to be(:trailer?)
      expect(stream.app_headers.fields).to be == [["x-request", "first"]]
    end
  end
end