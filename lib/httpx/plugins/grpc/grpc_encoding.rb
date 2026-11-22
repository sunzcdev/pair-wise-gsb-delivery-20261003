# frozen_string_literal: true

module HTTPX
  module Transcoder
    module GRPCEncoding
      class Deflater
        extend Forwardable

        attr_reader :content_type

        def initialize(body, compressed:)
          @content_type = body.content_type
          @body = BodyReader.new(body)
          @compressed = compressed
        end

        def bytesize
          return @body.bytesize if @body.respond_to?(:bytesize)

          Float::INFINITY
        end

        def read(length = nil, outbuf = nil)
          buf = @body.read(length, outbuf)

          return unless buf

          compressed_flag = @compressed ? 1 : 0

          buf = outbuf if outbuf

          buf = buf.b if buf.frozen?

          buf.prepend([compressed_flag, buf.bytesize].pack("CL>"))
          buf
        end
      end

      class Inflater
        # size in bytes of the GRPC length-prefixed message header
        # (1 compressed-flag byte + 4 big-endian length bytes)
        HEADER_SIZE = 5

        def initialize(response)
          @response = response
          @grpc_encodings = nil
          # bytes of an in-flight message which haven't been fully consumed yet
          @buffer = "".b
          # payload size advertised by the current message header (+nil+ while
          # waiting for a new message header)
          @payload_size = nil
          # compressed flag advertised by the current message header
          @compressed = false
        end

        # incrementally feeds a transport +chunk+ to the framer, yielding every
        # fully decoded GRPC message in arrival order. Message headers and payloads
        # may span an arbitrary number of chunks (a single header may even arrive
        # one byte at a time): incomplete data is buffered until more arrives.
        def call(chunk, &blk)
          @buffer << chunk

          loop do
            if @payload_size.nil?
              # still collecting the 5-byte message header
              return if @buffer.bytesize < HEADER_SIZE

              @compressed, @payload_size = @buffer.unpack("CL>")
              @buffer = @buffer.byteslice(HEADER_SIZE..-1) || "".b
            end

            # still collecting the message payload
            return if @buffer.bytesize < @payload_size

            encoded_data = @buffer.byteslice(0, @payload_size)
            @buffer = @buffer.byteslice(@payload_size..-1) || "".b

            decode_payload(encoded_data, &blk)

            # reset the framer for the next message
            @payload_size = nil
            @compressed = false
          end
        end

        # verifies that no half-delivered message remains buffered once the
        # transport stops emitting data, distinguishing a truncated message
        # header from a truncated message payload.
        def finish!
          return if @payload_size.nil? && @buffer.empty?

          if @payload_size.nil?
            raise GRPCMessageHeaderError.new(@buffer.bytesize)
          end

          raise GRPCMessagePayloadError.new(@payload_size, @buffer.bytesize)
        end

        private

        def decode_payload(encoded_data, &blk)
          if @compressed == 1
            grpc_encodings.reverse_each do |encoding|
              decoder = @response.body.class.initialize_inflater_by_encoding(encoding, @response, bytesize: encoded_data.bytesize)
              encoded_data = decoder.call(encoded_data)
            end
          end

          blk.call(encoded_data) if blk
        end

        def grpc_encodings
          @grpc_encodings ||= @response.headers.get("grpc-encoding")
        end
      end

      def self.encode(...)
        Deflater.new(...)
      end

      def self.decode(response)
        Inflater.new(response)
      end
    end
  end
end
