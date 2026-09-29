# frozen_string_literal: true

RSpec.describe JWT::VerificationReceipt do
  let(:payload) { { 'pay' => 'load', 'iss' => 'https://issuer.example' } }
  let(:encoded_token) { JWT::Token.new(payload: payload).tap { |t| t.sign!(algorithm: 'HS256', key: 'secret') }.jwt }

  subject(:token) { JWT::EncodedToken.new(encoded_token) }

  describe '.new' do
    it 'is not publicly constructible' do
      expect do
        described_class.new(token: 'token', algorithm: 'HS256', policy: :api, checks: [], verified_at: Time.now)
      end.to raise_error(NoMethodError)
    end
  end

  describe 'issuing via EncodedToken#verify_with_receipt!' do
    subject(:receipt) do
      token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' },
                                 claims: [:exp, { iss: 'https://issuer.example' }],
                                 policy: :admin_api)
    end

    it 'binds the raw token bytes' do
      expect(receipt.token).to eq(encoded_token)
    end

    it 'binds the algorithm from the token header' do
      expect(receipt.algorithm).to eq('HS256')
    end

    it 'binds the caller provided policy identifier' do
      expect(receipt.policy).to eq(:admin_api)
    end

    it 'records the verification time' do
      expect(receipt.verified_at).to be_within(5).of(Time.now)
    end

    it 'records the passed checks' do
      expect(receipt.checks).to eq([:signature, :exp, :iss])
    end

    it 'is immutable' do
      expect(receipt).to be_frozen
      expect(receipt.token).to be_frozen
      expect(receipt.checks).to be_frozen
    end

    context 'when claims are not given' do
      subject(:receipt) do
        token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :admin_api)
      end

      it 'records the default exp check' do
        expect(receipt.checks).to eq([:signature, :exp])
      end
    end

    context 'when the signature is invalid' do
      it 'raises and issues no receipt' do
        expect do
          token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'wrong' }, policy: :admin_api)
        end.to raise_error(JWT::VerificationError)
      end

      it 'leaves the payload inaccessible' do
        expect do
          token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'wrong' }, policy: :admin_api)
        end.to raise_error(JWT::VerificationError)
        expect { token.payload }.to raise_error(JWT::TokenError)
      end
    end

    context 'when the claims are invalid' do
      it 'raises and issues no receipt' do
        expect do
          token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' },
                                     claims: [{ iss: 'https://other.example' }],
                                     policy: :admin_api)
        end.to raise_error(JWT::InvalidIssuerError)
      end
    end

    context 'when the payload is only readable via #unverified_payload' do
      let(:encoded_token) { JWT::Token.new(payload: payload).tap { |t| t.sign!(algorithm: 'HS256', key: 'secret') }.jwt }

      it 'cannot be turned into a receipt without a successful verification' do
        expect(token.unverified_payload).to eq(payload)
        expect do
          token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'wrong' }, policy: :admin_api)
        end.to raise_error(JWT::VerificationError)
      end
    end
  end

  describe 'repeated verification' do
    it 're-verifies and issues a new receipt with a fresh verification time' do
      first = token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :api)
      second = token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :api)

      expect(second).not_to be(first)
      expect(second.verified_at).to be >= first.verified_at
    end

    it 'keeps previously issued receipts immutable records' do
      first = token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :api)
      expect { token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'wrong' }, policy: :api) }
        .to raise_error(JWT::VerificationError)

      expect(first.policy).to eq(:api)
      expect(first.checks).to eq([:signature, :exp])
    end
  end

  describe 'policy semantics' do
    let(:admin_receipt) do
      token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' },
                                 claims: [:exp, { iss: 'https://issuer.example' }],
                                 policy: :admin_api)
    end
    let(:public_receipt) do
      token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' },
                                 claims: [:exp],
                                 policy: :public_api)
    end

    it 'a receipt is only valid for the policy it was issued under' do
      expect(admin_receipt.valid_for?(:admin_api)).to be(true)
      expect(admin_receipt.valid_for?(:public_api)).to be(false)
    end

    it 'two policies verifying the same token produce distinct receipts' do
      expect(admin_receipt).not_to eq(public_receipt)
      expect(admin_receipt.token).to eq(public_receipt.token)
      expect(public_receipt.checks).not_to include(:iss)
    end

    it 'an old receipt cannot prove another policy passed' do
      expect(public_receipt.valid_for?(:admin_api)).to be(false)
      expect(public_receipt.covers?(:iss)).to be(false)
    end
  end

  describe '#covers?' do
    subject(:receipt) do
      token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :api)
    end

    it 'reports the checks that passed' do
      expect(receipt.covers?(:signature)).to be(true)
      expect(receipt.covers?(:exp)).to be(true)
      expect(receipt.covers?(:nbf)).to be(false)
    end
  end

  describe '#matches?' do
    subject(:receipt) do
      token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :api)
    end

    it 'matches the raw token bytes' do
      expect(receipt.matches?(encoded_token)).to be(true)
      expect(receipt.matches?(token)).to be(true)
      expect(receipt.matches?('other.token.bytes')).to be(false)
    end
  end

  describe 'equality' do
    subject(:receipt) do
      token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :api)
    end

    it 'is a value object based on all attributes' do
      expect(receipt).to eq(receipt)
      expect(receipt.hash).to eq(receipt.to_h.hash)
      expect(receipt).not_to eq('not a receipt')
    end

    it 'differs when the policy differs' do
      other = token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' }, policy: :other)
      expect(receipt).not_to eq(other)
    end
  end
end
