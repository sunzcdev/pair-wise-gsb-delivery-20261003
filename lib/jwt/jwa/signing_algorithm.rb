# frozen_string_literal: true

module JWT
  # JSON Web Algorithms
  module JWA
    # Base functionality for signing algorithms
    module SigningAlgorithm
      # Class methods for the SigningAlgorithm module
      module ClassMethods
        def register_algorithm(algo)
          ::JWT::JWA.register_algorithm(algo)
        end
      end

      def self.included(klass)
        klass.extend(ClassMethods)
      end

      attr_reader :alg

      def valid_alg?(alg_to_check)
        alg&.casecmp(alg_to_check)&.zero? == true
      end

      # Returns the name of the digest the data must be hashed with when
      # signing, or nil when the algorithm does not use a digest.
      def digest
        nil
      end

      # Returns the representation expected for signatures handed back from
      # an external signer. :raw means the native signature bytes,
      # :ecdsa_raw means the JWS raw R || S fixed-width concatenation.
      def signature_format
        :raw
      end

      # Returns the expected byte length of a signature, or nil when the
      # algorithm does not prescribe a fixed length.
      def signature_length
        nil
      end

      def header(*)
        { 'alg' => alg }
      end

      def sign(*)
        raise_sign_error!('Algorithm implementation is missing the sign method')
      end

      def verify(*)
        raise_verify_error!('Algorithm implementation is missing the verify method')
      end

      def raise_verify_error!(message)
        raise(VerificationKeyError.new(message).tap { |e| e.set_backtrace(caller(1)) })
      end

      def raise_sign_error!(message)
        raise(EncodeError.new(message).tap { |e| e.set_backtrace(caller(1)) })
      end
    end

    class << self
      def register_algorithm(algo)
        algorithms[algo.alg.to_s.downcase] = algo
      end

      def find(algo)
        algorithms.fetch(algo.to_s.downcase, Unsupported)
      end

      private

      def algorithms
        @algorithms ||= {}
      end
    end
  end
end
