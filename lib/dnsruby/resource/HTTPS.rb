require 'dnsruby/resource/SVCB'

module Dnsruby
  class RR
    # Class for DNS HTTPS resource records.
    #
    # RFC 9460 sec 9
    #
    # The HTTPS-specific instantiation of the SVCB record, sharing its wire and
    # presentation formats, for the "https" and "http" URI schemes.
    class HTTPS < SVCB
      ClassValue = nil #:nodoc: all
      TypeValue = Types::HTTPS #:nodoc: all
    end
  end
end
