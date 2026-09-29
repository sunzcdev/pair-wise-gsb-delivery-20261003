# frozen_string_literal: true

RSpec.describe 'verification receipt usage' do
  let(:payload) { { 'sub' => 'user-1', 'exp' => Time.now.to_i + 3600, 'aud' => 'billing' } }
  let(:jwt) { JWT::Token.new(payload: payload).tap { |t| t.sign!(algorithm: 'HS256', key: 'secret') }.jwt }

  # A cache keyed by token bytes alone. A cached receipt is only trusted when
  # it covers the requested policy, otherwise the token is verified again and
  # the fresh receipt replaces the cached one.
  def build_policy_cache
    cache = {}
    verifier = lambda do |token_bytes, policy, claims|
      cached = cache[token_bytes]
      return cached if cached && cached.covers?(policy)

      token = JWT::EncodedToken.new(token_bytes)
      cache[token_bytes] = token.verify_with_receipt!(policy: policy,
                                                      signature: { algorithm: 'HS256', key: 'secret' },
                                                      claims: claims)
    end
    [cache, verifier]
  end

  it 'a receipt issued for one policy does not prove another policy passed on the same token' do
    cache, verify_for = build_policy_cache

    # The :user policy only requires the exp claim.
    user_receipt = verify_for.call(jwt, :user, [:exp])
    expect(user_receipt.policy).to eq(:user)
    expect(user_receipt.checks).to eq([:signature, :exp])
    expect(cache[jwt]).to be(user_receipt)

    # The cached :user receipt must not be accepted as proof of the stricter
    # :admin policy, even though it was issued for the very same token bytes.
    expect(user_receipt.covers?(:admin)).to be(false)

    # Requesting the :admin policy therefore triggers a full re-verification
    # and issues a distinct receipt.
    admin_receipt = verify_for.call(jwt, :admin, [:exp, { aud: 'billing' }])
    expect(admin_receipt).not_to be(user_receipt)
    expect(admin_receipt).not_to eq(user_receipt)
    expect(admin_receipt.policy).to eq(:admin)
    expect(admin_receipt.checks).to eq([:signature, :exp, :aud])

    # Repeated verification under the same policy is served by the cache.
    expect(verify_for.call(jwt, :admin, [:exp, { aud: 'billing' }])).to be(admin_receipt)

    # And the old :user receipt still proves nothing about :admin.
    expect(user_receipt.covers?(:admin)).to be(false)
  end

  it 'a failed verification issues no receipt and grants no access' do
    token = JWT::EncodedToken.new(jwt)

    expect do
      token.verify_with_receipt!(policy: :admin,
                                 signature: { algorithm: 'HS256', key: 'secret' },
                                 claims: [{ aud: 'accounting' }])
    end.to raise_error(JWT::InvalidAudError)

    expect { token.payload }.to raise_error(JWT::TokenError)
  end
end
