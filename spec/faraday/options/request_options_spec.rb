# frozen_string_literal: true

RSpec.describe Faraday::RequestOptions do
  subject(:options) { Faraday::RequestOptions.new }

  it 'allows to set the request proxy' do
    expect(options.proxy).to be_nil

    expect { options[:proxy] = { booya: 1 } }.to raise_error(NoMethodError)

    options[:proxy] = { user: 'user' }
    expect(options.proxy).to be_a_kind_of(Faraday::ProxyOptions)
    expect(options.proxy.user).to eq('user')

    options.proxy = nil
    expect(options.proxy).to be_nil
    expect(options.inspect).to eq('#<Faraday::RequestOptions (empty)>')
  end

  describe 'declared shared members' do
    it 'declares the read-only collaborators' do
      expect(described_class::SHARED_MEMBERS).to contain_exactly(:params_encoder, :on_data)
    end

    it 'answers whether a member is shared' do
      expect(described_class.shared_member?(:params_encoder)).to be(true)
      expect(described_class.shared_member?('on_data')).to be(true)
      expect(described_class.shared_member?(:context)).to be(false)
      expect(described_class.shared_member?(:timeout)).to be(false)
    end
  end

  describe '.from' do
    it 'takes ownership of nested structures given as a hash' do
      source = { context: { nested: { list: [] } }, oauth: { consumer_key: 'anonymous' } }
      built = described_class.from(source)

      expect(built.context).to eq(source[:context])
      expect(built.context).not_to equal(source[:context])
      expect(built.context[:nested]).not_to equal(source[:context][:nested])
      expect(built.context[:nested][:list]).not_to equal(source[:context][:nested][:list])

      built.context[:nested][:list] << :built
      built.oauth[:consumer_secret] = 'secret'
      expect(source[:context][:nested][:list]).to eq([])
      expect(source[:oauth]).to eq(consumer_key: 'anonymous')
    end

    it 'returns an independent copy of another options instance' do
      original = described_class.from(context: { trace: [] })
      copy = described_class.from(original)

      copy.context[:trace] << :copy

      expect(original.context[:trace]).to eq([])
    end

    it 'keeps declared shared members by reference' do
      encoder = Faraday::FlatParamsEncoder
      callback = proc { |_chunk| }
      original = described_class.from(params_encoder: encoder, on_data: callback)

      copy = described_class.from(original)

      expect(copy.params_encoder).to equal(encoder)
      expect(copy.on_data).to equal(callback)
    end

    it 'returns empty options for nil' do
      expect(described_class.from(nil).to_hash).to eq({})
    end

    it 'raises for unsupported values, like the legacy Options' do
      expect { described_class.from(42) }.to raise_error(NoMethodError)
    end
  end

  describe '#deep_dup' do
    it 'copies caller-provided nested structures recursively' do
      original = described_class.from(
        context: { nested: { list: [1] } },
        oauth: { consumer_key: 'anonymous' },
        bind: { host: 'localhost', port: 80 }
      )

      copy = original.deep_dup

      expect(copy.to_hash).to eq(original.to_hash)
      expect(copy.context).not_to equal(original.context)
      expect(copy.context[:nested][:list]).not_to equal(original.context[:nested][:list])
      expect(copy.oauth).not_to equal(original.oauth)
      expect(copy.bind).not_to equal(original.bind)
    end

    it 'never changes the original when the copy is mutated' do
      original = described_class.from(context: { trace: [] }, oauth: { consumer_key: 'anonymous' })
      copy = original.deep_dup

      copy.context[:trace] << :copy
      copy.oauth[:consumer_secret] = 'secret'

      expect(original.context[:trace]).to eq([])
      expect(original.oauth).to eq(consumer_key: 'anonymous')
    end

    it 'keeps declared shared members by reference' do
      encoder = Faraday::FlatParamsEncoder
      callback = proc { |_chunk| }
      original = described_class.from(params_encoder: encoder, on_data: callback)

      copy = original.deep_dup

      expect(copy.params_encoder).to equal(encoder)
      expect(copy.on_data).to equal(callback)
    end

    it 'deep copies nested options instances' do
      original = described_class.new
      original[:proxy] = { uri: 'http://proxy.com:80' }

      copy = original.deep_dup

      expect(copy.proxy).to be_a(Faraday::ProxyOptions)
      expect(copy.proxy).not_to equal(original.proxy)
      expect(copy.proxy.host).to eq('proxy.com')
    end

    it 'preserves false values' do
      expect(described_class.from(timeout: false).deep_dup.to_hash).to eq(timeout: false)
    end
  end
end
