# frozen_string_literal: true

RSpec.describe JWT::VerificationReceipt do
  let(:payload) { { 'pay' => 'load', 'exp' => Time.now.to_i + 3600, 'aud' => 'billing' } }
  let(:encoded_token) do
    JWT::Token.new(payload: payload).tap { |t| t.sign!(algorithm: 'HS256', key: 'secret') }.jwt
  end

  subject(:token) { JWT::EncodedToken.new(encoded_token) }

  describe 'issuance via JWT::EncodedToken#verify_with_receipt!' do
    it 'returns a receipt binding the token bytes, algorithm and policy' do
      receipt = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })

      expect(receipt.jwt).to eq(encoded_token)
      expect(receipt.algorithm).to eq('HS256')
      expect(receipt.policy).to eq(:session)
      expect(receipt.verified_at).to be_within(2).of(Time.now)
      expect(receipt.checks).to eq([:signature, :exp])
    end

    it 'records the verified claim checks' do
      receipt = token.verify_with_receipt!(policy: :session,
                                           signature: { algorithm: 'HS256', key: 'secret' },
                                           claims: [:exp, { aud: 'billing' }])

      expect(receipt.checks).to eq([:signature, :exp, :aud])
    end

    it 'requires a policy identifier' do
      expect do
        token.verify_with_receipt!(policy: nil, signature: { algorithm: 'HS256', key: 'secret' })
      end.to raise_error(ArgumentError, 'A policy identifier is required to issue a verification receipt')
    end

    it 'grants payload access after a successful verification' do
      token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })

      expect(token.payload).to eq(payload.transform_keys(&:to_s))
    end
  end

  describe 'immutability' do
    let(:receipt) { token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' }) }

    it 'freezes the receipt and its mutable attributes' do
      expect(receipt).to be_frozen
      expect(receipt.jwt).to be_frozen
      expect(receipt.algorithm).to be_frozen
      expect(receipt.verified_at).to be_frozen
      expect(receipt.checks).to be_frozen
    end

    it 'cannot be modified' do
      expect { receipt.instance_variable_set(:@policy, :admin) }.to raise_error(FrozenError)
      expect { receipt.checks << :aud }.to raise_error(FrozenError)
    end

    it 'cannot be constructed directly' do
      expect(described_class).not_to respond_to(:new)
      expect do
        described_class.new(jwt: 'a.b.c', algorithm: 'HS256', policy: :session, verified_at: Time.now, checks: [])
      end.to raise_error(NoMethodError)
    end

    it 'cannot be minted from an unverified payload' do
      expect(token.unverified_payload).to eq(payload.transform_keys(&:to_s))
      expect { token.payload }.to raise_error(JWT::TokenError)
      expect(token).not_to respond_to(:verification_receipt)
    end
  end

  describe 'failed verification' do
    it 'raises and issues no receipt' do
      expect do
        token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'wrong' })
      end.to raise_error(JWT::VerificationError)
    end

    it 'leaves the payload inaccessible after a failed verification' do
      expect do
        token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'wrong' })
      end.to raise_error(JWT::VerificationError)

      expect { token.payload }.to raise_error(JWT::TokenError, 'Verify the token signature before accessing the payload')
    end

    it 'raises when claim verification fails' do
      expect do
        token.verify_with_receipt!(policy: :session,
                                   signature: { algorithm: 'HS256', key: 'secret' },
                                   claims: [{ aud: 'other' }])
      end.to raise_error(JWT::InvalidAudError)
    end
  end

  describe 'repeated verification' do
    it 'verifies fully again and issues a new receipt each time' do
      first  = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })
      second = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })

      expect(second).not_to be(first)
      expect(second.jwt).to eq(first.jwt)
      expect(second.policy).to eq(first.policy)
      expect(second.checks).to eq(first.checks)
    end

    it 'never mutates previously issued receipts' do
      first = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })
      token.verify_with_receipt!(policy: :admin, signature: { algorithm: 'HS256', key: 'secret' })

      expect(first.policy).to eq(:session)
    end
  end

  describe 'policy binding' do
    let(:receipt) { token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' }) }

    it 'covers only the policy it was issued for' do
      expect(receipt.covers?(:session)).to be(true)
      expect(receipt.covers?(:admin)).to be(false)
    end

    it 'is not equal to a receipt issued for another policy on the same token' do
      other = token.verify_with_receipt!(policy: :admin, signature: { algorithm: 'HS256', key: 'secret' })

      expect(receipt).not_to eq(other)
      expect(receipt).not_to eql(other)
    end

    it 'is equal to a receipt with identical attributes' do
      fixed_time = Time.now
      allow(Time).to receive(:now).and_return(fixed_time)

      first  = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })
      second = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })

      expect(first).to eq(second)
      expect(first.hash).to eq(second.hash)
    end
  end

  describe 'token binding' do
    it 'matches the token it was issued for' do
      receipt = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })

      expect(receipt.for_token?(token)).to be(true)
      expect(receipt.for_token?(JWT::EncodedToken.new(encoded_token))).to be(true)
    end

    it 'no longer matches when the token bytes change' do
      receipt = token.verify_with_receipt!(policy: :session, signature: { algorithm: 'HS256', key: 'secret' })
      token.encoded_payload = JWT::Token.new(payload: { 'other' => 'payload' }).encoded_payload

      expect(receipt.for_token?(token)).to be(false)
    end
  end
end
