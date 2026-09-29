# frozen_string_literal: true

# A one-shot latch used to hold a loader call until the test releases it.
class KeyFinderSpecLatch
  def initialize
    @mutex = Mutex.new
    @condition = ConditionVariable.new
    @open = false
  end

  def wait
    @mutex.synchronize { @condition.wait(@mutex) until @open }
  end

  def open
    @mutex.synchronize do
      @open = true
      @condition.broadcast
    end
  end
end

RSpec.describe JWT::JWK::KeyFinder do
  def oct_key(kid)
    { kid: kid, kty: 'oct', k: JWT::Base64.url_encode("secret-for-#{kid}") }
  end

  def secret_for(kid)
    "secret-for-#{kid}"
  end

  describe '#key_for' do
    it 'finds a known key without refreshing' do
      loader_calls = []
      loader = lambda do |options|
        loader_calls << options
        { keys: [oct_key('known-key')] }
      end
      finder = described_class.new(jwks: loader)

      expect(finder.key_for('known-key')).to eq(secret_for('known-key'))
      expect(loader_calls).to eq([{ kid: 'known-key' }])
    end

    it 'passes invalidation options to the loader when the kid is unknown' do
      loader_calls = []
      loader = lambda do |options|
        loader_calls << options
        { keys: [oct_key('known-key')] }
      end
      finder = described_class.new(jwks: loader)

      expect { finder.key_for('unknown-kid') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid unknown-kid')
      expect(loader_calls).to eq([
                                   { kid: 'unknown-kid' },
                                   { invalidate: true, kid_not_found: true, kid: 'unknown-kid' }
                                 ])
    end

    context 'when multiple threads miss the same unknown kid' do
      it 'triggers the loader only once' do
        entered = Queue.new
        latch = KeyFinderSpecLatch.new
        loader_calls = 0
        calls_lock = Mutex.new

        loader = lambda do |options|
          if options[:invalidate]
            calls_lock.synchronize { loader_calls += 1 }
            entered << true
            latch.wait
          end
          { keys: [oct_key('known-key')] }
        end

        finder = described_class.new(jwks: loader)

        threads = 5.times.map do
          Thread.new do
            begin
              finder.key_for('unknown-kid')
              :found
            rescue JWT::SignatureError => e
              e.message
            end
          end
        end

        entered.pop # the shared refresh is now inside the loader
        sleep 0.1   # give the other threads a chance to pile up on the refresh
        latch.open

        expect(threads.map(&:value)).to all(eq('Could not find public key for kid unknown-kid'))
        expect(loader_calls).to eq(1)
      end
    end

    context 'when the refresh fails while other threads wait' do
      it 'propagates the error to all waiting threads and calls the loader once' do
        entered = Queue.new
        latch = KeyFinderSpecLatch.new
        loader_calls = 0
        calls_lock = Mutex.new

        loader = lambda do |options|
          if options[:invalidate]
            calls_lock.synchronize { loader_calls += 1 }
            entered << true
            latch.wait
            raise 'jwks endpoint is down'
          end
          { keys: [oct_key('known-key')] }
        end

        finder = described_class.new(jwks: loader)

        threads = 5.times.map do
          Thread.new do
            begin
              finder.key_for('unknown-kid')
              :found
            rescue RuntimeError => e
              e.message
            end
          end
        end

        entered.pop
        sleep 0.1
        latch.open

        expect(threads.map(&:value)).to all(eq('jwks endpoint is down'))
        expect(loader_calls).to eq(1)
      end
    end

    context 'when the refresh fails' do
      it 'keeps the previously loaded keys available and does not cache the failure' do
        loader_calls = 0
        loader = lambda do |options|
          if options[:invalidate]
            loader_calls += 1
            raise 'jwks endpoint is down'
          end
          { keys: [oct_key('known-key')] }
        end

        finder = described_class.new(jwks: loader)

        expect(finder.key_for('known-key')).to eq(secret_for('known-key'))
        expect { finder.key_for('unknown-kid') }.to raise_error(RuntimeError, 'jwks endpoint is down')
        expect(finder.key_for('known-key')).to eq(secret_for('known-key'))

        expect { finder.key_for('unknown-kid') }.to raise_error(RuntimeError, 'jwks endpoint is down')
        expect(loader_calls).to eq(2)
      end
    end

    context 'when refreshes complete out of order' do
      it 'does not let an earlier refresh overwrite a newer successful result' do
        entered = Queue.new
        latch = KeyFinderSpecLatch.new
        loader_calls = 0
        calls_lock = Mutex.new

        loader = lambda do |options|
          return { keys: [] } unless options[:invalidate]

          calls_lock.synchronize { loader_calls += 1 }
          if options[:kid] == 'slow-kid'
            entered << true
            latch.wait
            { keys: [oct_key('slow-key')] }
          else
            { keys: [oct_key('fresh-kid')] }
          end
        end

        finder = described_class.new(jwks: loader)

        slow_result = Thread.new do
          begin
            finder.key_for('slow-kid')
          rescue JWT::SignatureError => e
            e.message
          end
        end

        entered.pop # the earlier refresh is now in flight

        # a newer refresh completes first and installs 'fresh-kid'
        expect(finder.key_for('fresh-kid')).to eq(secret_for('fresh-kid'))

        # the earlier refresh completes afterwards with a stale set
        latch.open
        expect(slow_result.value).to eq('Could not find public key for kid slow-kid')

        # the newer result is still in place
        expect(finder.key_for('fresh-kid')).to eq(secret_for('fresh-kid'))

        # the stale set never became visible: 'slow-key' is still unknown
        expect { finder.key_for('slow-key') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid slow-key')
        expect(loader_calls).to eq(3)
      end
    end

    context 'with the unknown kid cache' do
      it 'remembers an unknown kid for the configured ttl' do
        loader_calls = 0
        loader = lambda do |options|
          loader_calls += 1 if options[:invalidate]
          { keys: [oct_key('known-key')] }
        end

        finder = described_class.new(jwks: loader, unknown_kid_cache_ttl: 0.1)

        expect { finder.key_for('unknown-kid') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid unknown-kid')
        expect { finder.key_for('unknown-kid') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid unknown-kid')
        expect(loader_calls).to eq(1)

        sleep 0.2

        expect { finder.key_for('unknown-kid') }.to raise_error(JWT::SignatureError, 'Could not find public key for kid unknown-kid')
        expect(loader_calls).to eq(2)
      end

      it 'bounds the number of remembered unknown kids' do
        loader_calls = 0
        loader = lambda do |options|
          loader_calls += 1 if options[:invalidate]
          { keys: [oct_key('known-key')] }
        end

        finder = described_class.new(jwks: loader, unknown_kid_cache_size: 2, unknown_kid_cache_ttl: 60)

        %w[kid-one kid-two kid-three].each do |kid|
          expect { finder.key_for(kid) }.to raise_error(JWT::SignatureError, "Could not find public key for kid #{kid}")
        end
        expect(loader_calls).to eq(3)

        # kid-two and kid-three are still remembered
        expect { finder.key_for('kid-two') }.to raise_error(JWT::SignatureError)
        expect(loader_calls).to eq(3)

        # kid-one was evicted when kid-three was remembered
        expect { finder.key_for('kid-one') }.to raise_error(JWT::SignatureError)
        expect(loader_calls).to eq(4)
      end
    end
  end
end
