# frozen_string_literal: true

RSpec.describe Faraday::Env do
  subject(:env) { described_class.new }

  it 'allows to access members' do
    expect(env.method).to be_nil
    env.method = :get
    expect(env.method).to eq(:get)
  end

  it 'allows to access symbol non members' do
    expect(env[:custom]).to be_nil
    env[:custom] = :boom
    expect(env[:custom]).to eq(:boom)
  end

  it 'allows to access string non members' do
    expect(env['custom']).to be_nil
    env['custom'] = :boom
    expect(env['custom']).to eq(:boom)
  end

  it 'ignores false when fetching' do
    ssl = Faraday::SSLOptions.new
    ssl.verify = false
    expect(ssl.fetch(:verify, true)).to be_falsey
  end

  it 'handle verify_hostname when fetching' do
    ssl = Faraday::SSLOptions.new
    ssl.verify_hostname = true
    expect(ssl.fetch(:verify_hostname, false)).to be_truthy
  end

  it 'retains custom members' do
    env[:foo] = 'custom 1'
    env[:bar] = :custom2
    env2 = Faraday::Env.from(env)
    env2[:baz] = 'custom 3'

    expect(env2[:foo]).to eq('custom 1')
    expect(env2[:bar]).to eq(:custom2)
    expect(env[:baz]).to be_nil
  end

  describe '#body' do
    subject(:env) { described_class.from(body: { foo: 'bar' }) }

    context 'when response is not finished yet' do
      it 'returns the request body' do
        expect(env.body).to eq(foo: 'bar')
      end
    end

    context 'when response is finished' do
      before do
        env.status = 200
        env.body = { bar: 'foo' }
        env.response = Faraday::Response.new(env)
      end

      it 'returns the response body' do
        expect(env.body).to eq(bar: 'foo')
      end

      it 'allows to access request_body' do
        expect(env.request_body).to eq(foo: 'bar')
      end

      it 'allows to access response_body' do
        expect(env.response_body).to eq(bar: 'foo')
      end
    end
  end

  describe '#stream_response' do
    subject(:env) do
      env = described_class.new
      env.request = Faraday::RequestOptions.new
      env.request.on_data = on_data
      env
    end

    let(:streamed) { [] }
    let(:on_data) do
      proc { |chunk, size, block_env| streamed << [chunk, size, block_env] }
    end

    it 'passes a writer to a positional block argument' do
      env.stream_response do |writer|
        writer.call('ab')
        writer.call('cde')
      end

      expect(streamed.map { |chunk, size, _| [chunk, size] }).to eq([
        ['ab', 2],
        ['cde', 5]
      ])
      expect(streamed.map { |_, _, block_env| block_env }.uniq).to eq([env])
    end

    it 'passes the writer to a block-capturing argument (&on_data style)' do
      env.stream_response do |&writer|
        writer.call('xyz')
      end

      expect(streamed.map { |chunk, size, _| [chunk, size] }).to eq([['xyz', 3]])
    end

    it 'invokes on_data once with an empty chunk when nothing is streamed' do
      env.stream_response { |writer| writer.call('') }

      expect(streamed).to eq([['', 0, env]])
    end
  end
end
