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

      def header(*)
        { 'alg' => alg }
      end

      # Returns the name of the digest an external signer must apply to the
      # signing input, or nil when the algorithm does not prescribe one.
      #
      # @return [String, nil] the OpenSSL digest name (e.g. "SHA256").
      def digest
        nil
      end

      # Validates and normalizes a signature produced by an external signer
      # into the representation used inside a JWT.
      #
      # @param signature [String] the signature returned by the external signer.
      # @param format [Symbol] the representation of the given signature.
      #   Algorithms supporting multiple representations accept algorithm
      #   specific formats, others only accept +:raw+ (the exact JWS bytes).
      # @return [String] the normalized signature, ready to be embedded in a token.
      def normalize_external_signature(signature, format: :raw)
        raise_sign_error!("Unsupported signature format #{format.inspect} for #{alg}. Only :raw is supported") unless format == :raw
        raise_sign_error!('Signature must be a String') unless signature.is_a?(String)

        signature
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
