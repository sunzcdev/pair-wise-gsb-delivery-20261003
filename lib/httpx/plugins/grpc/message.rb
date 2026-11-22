# frozen_string_literal: true

module HTTPX
  module Plugins
    module GRPC
      # Encoding module for GRPC responses
      #
      # Can encode and decode grpc messages.
      module Message
        module_function

        # decodes a unary grpc response
        def unary(response)
          verify_status(response)

          decoder = Transcoder::GRPCEncoding.decode(response)

          data = "".b
          response.each do |frame|
            decoder.call(frame) { |message| data << message }
          end
          decoder.finish!

          data
        end

        # lazy decodes a grpc stream response
        def stream(response, &block)
          return enum_for(__method__, response) unless block

          decoder = Transcoder::GRPCEncoding.decode(response)

          response.each do |frame|
            decoder.call(frame, &block)
          end

          # the transport stopped before the in-flight GRPC message was fully framed
          decoder.finish!

          verify_status(response)
        end

        def cancel(request)
          request.emit(:refuse, :client_cancellation)
        end

        # interprets the grpc call trailing metadata, and raises an
        # exception in case of error code
        def verify_status(response)
          # return standard errors if need be
          response.raise_for_status

          status = Integer(response.headers["grpc-status"])
          message = response.headers["grpc-message"]

          return if status.zero?

          response.close
          raise GRPCError.new(status, message, response.trailing_metadata)
        end
      end
    end
  end
end
