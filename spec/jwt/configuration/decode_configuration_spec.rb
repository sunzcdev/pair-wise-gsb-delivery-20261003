# frozen_string_literal: true

RSpec.describe JWT::Configuration::DecodeConfiguration do
  subject(:config) { described_class.new }

  it 'has the time constraints verification disabled by default' do
    expect(config.verify_time_constraints).to be(false)
  end

  it 'has no maximum token lifetime configured by default' do
    expect(config.max_token_lifetime).to be_nil
  end

  it 'has no clock configured by default' do
    expect(config.clock).to be_nil
  end

  it 'exposes the new options in the options hash' do
    config.clock = -> { Time.now }
    config.max_token_lifetime = 60
    config.verify_time_constraints = { max_token_lifetime: 60 }

    expect(config.to_h).to include(
      clock: config.clock,
      max_token_lifetime: 60,
      verify_time_constraints: { max_token_lifetime: 60 }
    )
  end
end
