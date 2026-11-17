require 'concurrent/atomic/atomic_markable_reference'

RSpec.describe Concurrent::AtomicMarkableReference do
  subject { described_class.new 1000, true }

  describe '.initialize' do
    it 'constructs the object' do
      expect(subject.value).to eq 1000
      expect(subject.marked?).to eq true
    end

    it 'has sane defaults' do
      amr = described_class.new

      expect(amr.value).to eq nil
      expect(amr.marked?).to eq false
    end
  end

  describe '#set' do
    it 'sets the value and mark' do
      val, mark = subject.set 1001, true

      expect(subject.value).to eq 1001
      expect(subject.marked?).to eq true
      expect(val).to eq 1001
      expect(mark).to eq true
    end
  end

  describe '#try_update!' do
    it 'updates the value and mark' do
      val, mark = subject.try_update! { |v, m| [v + 1, !m] }

      expect(subject.value).to eq 1001
      expect(val).to eq 1001
      expect(mark).to eq false
    end

    it 'raises ConcurrentUpdateError when attempting to set inside of block' do
      expect do
        subject.try_update! do |v, m|
          subject.set(1001, false)
          [v + 1, !m]
        end
      end.to raise_error Concurrent::ConcurrentUpdateError
    end
  end

  describe '#try_update' do
    it 'updates the value and mark' do
      val, mark = subject.try_update { |v, m| [v + 1, !m] }

      expect(subject.value).to eq 1001
      expect(val).to eq 1001
      expect(mark).to eq false
    end

    it 'returns nil when attempting to set inside of block' do
      expect do
        subject.try_update do |v, m|
          subject.set(1001, false)
          [v + 1, !m]
        end.to eq nil
      end
    end
  end

  describe '#update' do
    it 'updates the value and mark' do
      val, mark = subject.update { |v, m| [v + 1, !m] }

      expect(subject.value).to eq 1001
      expect(subject.marked?).to eq false

      expect(val).to eq 1001
      expect(mark).to eq false
    end

    it 'retries until update succeeds' do
      tries = 0

      subject.update do |v, m|
        tries += 1
        subject.set(1001, false)
        [v + 1, !m]
      end

      expect(tries).to eq 2
    end
  end

  describe '#compare_and_set_observed' do
    it 'returns true and the installed pair on success' do
      success, val, mark = subject.compare_and_set_observed(1000, 1001, true, false)

      expect(success).to eq true
      expect(val).to eq 1001
      expect(mark).to eq false
      expect(subject.get).to eq [1001, false]
    end

    it 'returns false and the actually observed pair when expectation mismatches' do
      success, val, mark = subject.compare_and_set_observed(999, 1001, true, false)

      expect(success).to eq false
      expect(val).to eq 1000
      expect(mark).to eq true
      expect(subject.get).to eq [1000, true]
    end

    it 'returns false and the observed pair when the mark mismatches' do
      success, val, mark = subject.compare_and_set_observed(1000, 1001, false, true)

      expect(success).to eq false
      expect(val).to eq 1000
      expect(mark).to eq true
    end

    it 'returns a frozen triple' do
      expect(subject.compare_and_set_observed(1000, 1001, true, false)).to be_frozen
    end

    it 'follows the same comparison protocol as #compare_and_set' do
      subject.set([1, 2, 3], true)
      success, = subject.compare_and_set_observed([1, 2, 3], nil, true, false)
      expect(success).to eq false

      subject.set(2**100, false)
      success, = subject.compare_and_set_observed(2**100, 2**99, false, true)
      expect(success).to eq true
    end

    it 'always reports a pair that actually existed, under contention' do
      amr   = described_class.new 0, true
      pairs = (0..100).map { |i| [i, i.even?] }.freeze

      observed = Queue.new

      writers = 4.times.map do |t|
        Thread.new do
          25.times do |i|
            n = t * 25 + i + 1
            observed << amr.compare_and_set_observed(n - 1, n, (n - 1).even?, n.even?)
          end
        end
      end

      readers = 4.times.map do
        Thread.new do
          200.times do
            observed << amr.compare_and_set_observed(-1, -1, true, true)
          end
        end
      end

      (writers + readers).each(&:join)

      until observed.empty?
        _success, val, mark = observed.pop
        # The reported combination must be one that genuinely existed,
        # never a stitch of two separate reads.
        expect(pairs).to include([val, mark])
      end
    end
  end

  describe '#try_update_observed' do
    it 'returns true and the new pair on success' do
      success, val, mark = subject.try_update_observed { |v, m| [v + 1, !m] }

      expect(success).to eq true
      expect(val).to eq 1001
      expect(mark).to eq false
      expect(subject.get).to eq [1001, false]
    end

    it 'returns false and the observed pair on failure, invoking the block exactly once' do
      tries = 0

      success, val, mark = subject.try_update_observed do |v, m|
        tries += 1
        subject.set(1001, false)
        [v + 1, !m]
      end

      expect(tries).to eq 1
      expect(success).to eq false
      # The reported pair is the one observed by a single atomic snapshot,
      # not a combination stitched from separate reads.
      expect([val, mark]).to eq subject.get
    end

    it 'returns a frozen triple' do
      expect(subject.try_update_observed { |v, m| [v + 1, !m] }).to be_frozen
    end
  end

  describe '#compare_and_set' do
    context 'when objects have the same identity' do
      it 'sets the value and mark' do
        arr = [1, 2, 3]
        subject.set(arr, true)
        expect(subject.compare_and_set(arr, 1.2, true, false)).to be_truthy
      end
    end

    context 'when objects have the different identity' do
      it 'it does not set the value or mark' do
        subject.set([1, 2, 3], true)
        expect(subject.compare_and_set([1, 2, 3], 1.2, true, false))
          .to be_falsey
      end

      context 'when comparing Numeric objects' do
        context 'Non-idepotent Float' do
          it 'sets the value and mark' do
            subject.set(1.0 + 0.1, true)
            expect(subject.compare_and_set(1.0 + 0.1, 1.2, true, false))
              .to be_truthy
          end
        end

        context 'BigNum' do
          it 'sets the value and mark' do
            subject.set(2**100, false)
            expect(subject.compare_and_set(2**100, 2**99, false, true))
              .to be_truthy
          end
        end

        context 'Rational' do
          it 'sets the value and mark' do
            require 'rational' unless ''.respond_to? :to_r
            subject.set(Rational(1, 3), true)
            comp = subject.compare_and_set(Rational(1, 3),
                                           Rational(3, 1),
                                           true,
                                           false)
            expect(comp).to be_truthy
          end
        end
      end

      context 'Rational' do
        it 'is successful' do
          # Complex
          require 'complex' unless ''.respond_to? :to_c
          subject.set(Complex(1, 2), false)
          comp = subject.compare_and_set(Complex(1, 2),
                                         Complex(1, 3),
                                         false,
                                         true)
          expect(comp)
            .to be_truthy
        end
      end
    end
  end
end
