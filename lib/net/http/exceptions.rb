# frozen_string_literal: true
module Net
  # Net::HTTP exception class.
  # You cannot use Net::HTTPExceptions directly; instead, you must use
  # its subclasses.
  module HTTPExceptions # :nodoc:
    def initialize(msg, res)   #:nodoc:
      super msg
      @response = res
    end
    attr_reader :response
    alias data response    #:nodoc: obsolete
  end

  # :stopdoc:
  class HTTPError < ProtocolError
    include HTTPExceptions
  end

  class HTTPRetriableError < ProtoRetriableError
    include HTTPExceptions
  end

  class HTTPClientException < ProtoServerError
    include HTTPExceptions
  end

  class HTTPFatalError < ProtoFatalError
    include HTTPExceptions
  end

  # :stopdoc:
  # Mixed into exceptions raised while a connection is being established, so
  # that the failing layer can be attributed: +:transport+, +:proxy_tls+,
  # +:proxy_connect+ or +:target_tls+.
  #
  # The exception object keeps its original class (and, for an HTTP error,
  # its +response+), so existing rescue clauses keep working; only this
  # diagnostic is added.  The same value is available from
  # Net::HTTP#last_connect_stage.
  module HTTPConnectStage
    attr_reader :stage
  end
  # :startdoc:

  # We cannot use the name "HTTPServerError", it is the name of the response.
  HTTPServerException = HTTPClientException # :nodoc:
  deprecate_constant(:HTTPServerException)
end
