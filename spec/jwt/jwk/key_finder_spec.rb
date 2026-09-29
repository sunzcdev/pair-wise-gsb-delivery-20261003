# frozen_string_literal: true

require 'timeout'

RSpec.describe JWT::JWK::KeyFinder do
  # Builds a JWK oct hash for the given kid, the verify key resolves to secret
  def oct_key(kid, secret)
    { kty: 'oct', k: JWT::Base64.url_encode(secret), kid: kid }
  end

  let(:known_key) { oct_key('kid-known', 'known-secret') }
  let(:known_jwks) { { keys: [known_key] } }

  # A loader stub recording every call. Invalidate (refresh) calls can be
  # blocked on gates controlled by the test to simulate slow, failing and
  # out-of-order refreshes.
  class BarrierLoader
    attr_reader :calls, :invalidate_calls

    def initialize
      @calls = []
      @invalidate_calls = []
      @mutex = Mutex.new
      @default_response = nil
      @refresh_handler = nil
    end

    # Defines the response for calls without the invalidate option and the
    # fallback response for refresh calls without a refresh handler.
    def respond_with(jwks)
      @default_response = jwks
    end

    # Defines the behaviour of refresh (invalidate) calls. The block receives
    # the loader options and its return value is passed back to the KeyFinder.
    def on_refresh(&block)
      @refresh_handler = block
    end

    def call(options)
      @mutex.synchronize do
        @calls << options
        @invalidate_calls << options if options[:invalidate]
      end

      if options[:invalidate] && @refresh_handler
        @refresh_handler.call(options)
      else
        @default_response
      end
    end
  end

  let(:loader) { BarrierLoader.new }
  let(:key_finder) { described_class.new(jwks: loader, **finder_options) }
  let(:finder_options) { {} }

  # Pops from a gate, failing the example instead of blocking forever when
  # the expected loader interaction never happens.
  def wait_for_gate(queue)
    Timeout.timeout(5) { queue.pop }
  end

  describe 'initial loading' do
    it 'calls the loader only once when multiple threads resolve keys concurrently' do
      loader.respond_with(known_jwks)

      results = 5.times.map { Thread.new { key_finder.key_for('kid-known') } }.map(&:value)

      expect(results).to all(eq('known-secret'))
      expect(loader.calls.size).to eq(1)
    end
  end

  describe 'refresh coordination' do
    let(:new_key) { oct_key('kid-new', 'new-secret') }

    it 'performs a single refresh when multiple threads miss the same unknown kid' do
      refresh_entered = Queue.new
      release_refresh = Queue.new

      loader.respond_with(known_jwks)
      loader.on_refresh do |_options|
        refresh_entered << true
        wait_for_gate(release_refresh)
        { keys: [known_key, new_key] }
      end

      threads = 5.times.map { Thread.new { key_finder.key_for('kid-new') } }

      wait_for_gate(refresh_entered) # wait until the single refresh is inside the loader
      release_refresh << true

      expect(threads.map(&:value)).to all(eq('new-secret'))
      expect(loader.invalidate_calls.size).to eq(1)
      expect(loader.calls.size).to eq(2) # one initial load + one coordinated refresh
    end
  end

  describe 'refresh failures' do
    it 'keeps the previously fetched keys and negatively caches the unknown kid' do
      loader.respond_with(known_jwks)
      loader.on_refresh { raise IOError, 'network is down' }

      expect(key_finder.key_for('kid-known')).to eq('known-secret')

      expect { key_finder.key_for('kid-unknown') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid kid-unknown')
      expect(loader.invalidate_calls.size).to eq(1)

      # The unknown kid is negatively cached, no new refresh is triggered
      expect { key_finder.key_for('kid-unknown') }.to raise_error(JWT::SignatureError)
      expect(loader.invalidate_calls.size).to eq(1)

      # The previously fetched keys are still usable
      expect(key_finder.key_for('kid-known')).to eq('known-secret')
    end
  end

  describe 'negative cache' do
    before do
      loader.respond_with(known_jwks)
    end

    context 'when the ttl has not expired' do
      let(:finder_options) { { negative_cache_ttl: 60 } }

      it 'does not call the loader again for a repeated unknown kid' do
        2.times do
          expect { key_finder.key_for('kid-unknown') }.to raise_error(JWT::SignatureError)
        end

        expect(loader.invalidate_calls.size).to eq(1)
      end
    end

    context 'when the ttl has expired' do
      let(:finder_options) { { negative_cache_ttl: 0.05 } }

      it 'calls the loader again for the same unknown kid' do
        expect { key_finder.key_for('kid-unknown') }.to raise_error(JWT::SignatureError)
        expect(loader.invalidate_calls.size).to eq(1)

        sleep(0.1)

        expect { key_finder.key_for('kid-unknown') }.to raise_error(JWT::SignatureError)
        expect(loader.invalidate_calls.size).to eq(2)
      end
    end

    context 'when the capacity is exceeded' do
      let(:finder_options) { { negative_cache_capacity: 3, negative_cache_ttl: 60 } }

      it 'evicts the oldest unknown kids' do
        %w[kid-a kid-b kid-c kid-d].each do |kid|
          expect { key_finder.key_for(kid) }.to raise_error(JWT::SignatureError)
        end
        expect(loader.invalidate_calls.size).to eq(4)

        # kid-b, kid-c and kid-d are still cached, kid-a was evicted
        %w[kid-b kid-c kid-d].each do |kid|
          expect { key_finder.key_for(kid) }.to raise_error(JWT::SignatureError)
        end
        expect(loader.invalidate_calls.size).to eq(4)

        expect { key_finder.key_for('kid-a') }.to raise_error(JWT::SignatureError)
        expect(loader.invalidate_calls.size).to eq(5)
      end
    end
  end

  describe 'out-of-order refreshes' do
    let(:key_a) { oct_key('kid-a', 'secret-a') }
    let(:key_b) { oct_key('kid-b', 'secret-b') }

    it 'does not let an earlier refresh overwrite a newer successful result' do
      refresh_a_entered = Queue.new
      release_refresh_a = Queue.new

      loader.respond_with(known_jwks)
      loader.on_refresh do |options|
        if options[:kid] == 'kid-a'
          # The earlier issued refresh is slow and returns a stale snapshot
          refresh_a_entered << true
          wait_for_gate(release_refresh_a)
          { keys: [known_key, key_a] }
        else
          # The newer refresh returns first
          { keys: [known_key, key_b] }
        end
      end

      expect(key_finder.key_for('kid-known')).to eq('known-secret')

      thread_a = Thread.new do
        key_finder.key_for('kid-a')
      rescue JWT::SignatureError
        :not_found
      end

      wait_for_gate(refresh_a_entered) # the refresh for kid-a (issued first) is in flight

      thread_b = Thread.new { key_finder.key_for('kid-b') }
      expect(thread_b.value).to eq('secret-b') # newer refresh applied first

      release_refresh_a << true
      expect(thread_a.value).to eq(:not_found) # stale result was discarded

      expect(loader.invalidate_calls.map { |options| options[:kid] }).to eq(%w[kid-a kid-b])

      # The final key set is the one from the newer refresh
      expect(key_finder.key_for('kid-known')).to eq('known-secret')
      expect(key_finder.key_for('kid-b')).to eq('secret-b')
      expect { key_finder.key_for('kid-a') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid kid-a')
      expect(loader.invalidate_calls.size).to eq(2)
    end
  end
end
