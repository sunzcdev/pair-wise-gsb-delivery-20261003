# frozen_string_literal: true

module Dnsruby
  # RRSIG times are serial numbers, not absolute Unix timestamps (RFC 4034
  # section 3.1.5). Resolve each field into the era nearest the current time
  # before comparing it with ordinary, absolute cache deadlines.
  module SerialTime #:nodoc: all
    MASK = (1 << 32) - 1
    HALF = 1 << 31

    def self.absolute(value, reference)
      distance = (value.to_i - reference.to_i) & MASK
      # RFC 1982 deliberately leaves this ordering undefined.
      return nil if distance == HALF
      distance -= MASK + 1 if distance > HALF
      reference.to_i + distance
    end

    def self.valid?(inception, expiration, now = Time.now.to_i)
      starts = absolute(inception, now)
      ends = absolute(expiration, now)
      starts && ends && starts <= now && now <= ends
    end
  end
end
