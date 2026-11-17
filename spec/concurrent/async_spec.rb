require 'concurrent/async'

module Concurrent
  RSpec.describe Async do

    let(:async_class) do
      Class.new do
        include Concurrent::Async
        attr_accessor :accessor
        def initialize(*args)
        end
        def echo(msg)
          msg
        end
        def gather(first, second = nil)
          return first, second
        end
        def boom(ex = StandardError.new)
          raise ex
        end
        def wait(seconds)
          sleep(seconds)
        end
        def with_block
          yield
        end
      end
    end

    subject do
      async_class.new
    end

    context 'object creation' do

      it 'delegates to the original constructor' do
        args = [:foo, 'bar', 42]
        expect(async_class).to receive(:original_new).once.with(*args).and_call_original
        async_class.new(*args)
      end

      specify 'passes all args to the original constructor' do
        clazz = Class.new do
          include Concurrent::Async
          attr_reader :args
          def initialize(*args)
            @args = args
          end
        end

        object = clazz.new(:foo, :bar)
        expect(object.args).to eq [:foo, :bar]
      end

      specify 'passes a given block to the original constructor' do
        clazz = Class.new do
          include Concurrent::Async
          attr_reader :block
          def initialize(&block)
            @block = yield
          end
        end

        object = clazz.new{ 42 }
        expect(object.block).to eq 42
      end

      specify 'initializes synchronization' do
        mock = async_class.new
        allow(async_class).to receive(:original_new).and_return(mock)
        expect(mock).to receive(:init_synchronization).once.with(no_args)
        async_class.new
      end
    end

    context '#validate_argc' do

      subject do
        Class.new {
          def zero() nil; end
          def three(a, b, c, &block) nil; end
          def two_plus_two(a, b, c=nil, d=nil, &block) nil; end
          def many(*args, &block) nil; end
        }.new
      end

      it 'raises an exception when the method is not defined' do
        expect {
          Async::validate_argc(subject, :bogus)
        }.to raise_error(StandardError)
      end

      it 'raises an exception for too many args on a zero arity method' do
        expect {
          Async::validate_argc(subject, :zero, 1, 2, 3)
        }.to raise_error(ArgumentError)
      end

      it 'does not raise an exception for correct zero arity' do
        expect {
          Async::validate_argc(subject, :zero)
        }.not_to raise_error
      end

      it 'raises an exception for too many args on a method with positive arity' do
        expect {
          Async::validate_argc(subject, :three, 1, 2, 3, 4)
        }.to raise_error(ArgumentError)
      end

      it 'raises an exception for too few args on a method with positive arity' do
        expect {
          Async::validate_argc(subject, :three, 1, 2)
        }.to raise_error(ArgumentError)
      end

      it 'does not raise an exception for correct positive arity' do
        expect {
          Async::validate_argc(subject, :three, 1, 2, 3)
        }.not_to raise_error
      end

      it 'raises an exception for too few args on a method with negative arity' do
        expect {
          Async::validate_argc(subject, :two_plus_two, 1)
        }.to raise_error(ArgumentError)
      end

      it 'does not raise an exception for correct negative arity' do
        expect {
          Async::validate_argc(subject, :two_plus_two, 1, 2)
          Async::validate_argc(subject, :two_plus_two, 1, 2, 3, 4)
          Async::validate_argc(subject, :two_plus_two, 1, 2, 3, 4, 5, 6)

          Async::validate_argc(subject, :many)
          Async::validate_argc(subject, :many, 1, 2)
          Async::validate_argc(subject, :many, 1, 2, 3, 4)
        }.not_to raise_error
      end
    end

    context '#async' do

      it 'raises an error when calling a method that does not exist' do
        expect {
          subject.async.bogus
        }.to raise_error(StandardError)
      end

      it 'raises an error when passing too few arguments' do
        expect {
          subject.async.gather
        }.to raise_error(ArgumentError)
      end

      it 'raises an error when passing too many arguments (arity >= 0)' do
        expect {
          subject.async.echo(1, 2, 3, 4, 5)
        }.to raise_error(StandardError)
      end

      it 'returns the existence of the method' do
        expect(subject.async.respond_to?(:echo)).to be_truthy
        expect(subject.async.respond_to?(:not_exist_method)).to be_falsy
      end

      it 'returns a :pending IVar' do
        val = subject.async.wait(1)
        expect(val).to be_a Concurrent::IVar
        expect(val).to be_pending
      end

      it 'runs the future on the global executor' do
        expect(Concurrent.global_io_executor).to receive(:post).with(any_args).
          and_call_original
        subject.async.echo(:foo)
      end

      it 'sets the value on success' do
        val = subject.async.echo(:foo)
        expect(val.value).to eq :foo
        expect(val).to be_fulfilled
      end

      it 'sets the reason on failure' do
        ex = ArgumentError.new
        val = subject.async.boom(ex)
        val.wait
        expect(val.reason).to eq ex
        expect(val).to be_rejected
      end

      it 'sets the reason when giving too many optional arguments' do
        val = subject.async.gather(1, 2, 3, 4, 5)
        val.wait
        expect(val.reason).to be_a StandardError
        expect(val).to be_rejected
      end

      it 'supports attribute accessors' do
        subject.async.accessor = :foo
        val = subject.async.accessor
        expect(val.value).to eq :foo
        expect(subject.accessor).to eq :foo
      end

      it 'supports methods with blocks' do
        val = subject.async.with_block{ :foo }
        expect(val.value).to eq :foo
      end
    end

    context '#await' do

      it 'raises an error when calling a method that does not exist' do
        expect {
          subject.await.bogus
        }.to raise_error(StandardError)
      end

      it 'raises an error when passing too few arguments' do
        expect {
          subject.await.gather
        }.to raise_error(ArgumentError)
      end

      it 'raises an error when passing too many arguments (arity >= 0)' do
        expect {
          subject.await.echo(1, 2, 3, 4, 5)
        }.to raise_error(StandardError)
      end

      it 'returns the existence of the method' do
        expect(subject.await.respond_to?(:echo)).to be_truthy
        expect(subject.await.respond_to?(:not_exist_method)).to be_falsy
      end

      it 'returns a :fulfilled IVar' do
        val = subject.await.echo(5)
        expect(val).to be_a Concurrent::IVar
        expect(val).to be_fulfilled
      end

      it 'runs the future on the global executor' do
        expect(Concurrent.global_io_executor).to receive(:post).with(any_args).
          and_call_original
        subject.await.echo(:foo)
      end

      it 'sets the value on success' do
        val = subject.await.echo(:foo)
        expect(val.value).to eq :foo
        expect(val).to be_fulfilled
      end

      it 'sets the reason on failure' do
        ex = ArgumentError.new
        val = subject.await.boom(ex)
        expect(val.reason).to eq ex
        expect(val).to be_rejected
      end

      it 'sets the reason when giving too many optional arguments' do
        val = subject.await.gather(1, 2, 3, 4, 5)
        expect(val.reason).to be_a StandardError
        expect(val).to be_rejected
      end

      it 'supports attribute accessors' do
        subject.await.accessor = :foo
        val = subject.await.accessor
        expect(val.value).to eq :foo
        expect(subject.accessor).to eq :foo
      end

      it 'supports methods with blocks' do
        val = subject.await.with_block{ :foo }
        expect(val.value).to eq :foo
      end
    end

    context 'locking' do

      it 'uses the same lock for both #async and #await' do
        object = Class.new {
          include Concurrent::Async
          attr_reader :bucket
          def gather(seconds, first, *rest)
            sleep(seconds)
            (@bucket ||= []).concat([first])
            @bucket.concat(rest)
          end
        }.new

        object.async.gather(0.5, :a, :b)
        object.await.gather(0, :c, :d)
        expect(object.bucket).to eq [:a, :b, :c, :d]
      end
    end

    context 'fork safety' do
      it 'does not hang when forked' do
        skip "Platform does not support fork" unless Process.respond_to?(:fork)
        object = Class.new {
          include Concurrent::Async
          def foo; end
        }.new
        object.async.foo
        _, status = Process.waitpid2(fork {object.await.foo})
        expect(status.exitstatus).to eq 0
      end
    end

    context 'optional pre-submission signature check' do

      let(:validated_class) do
        Class.new do
          include Concurrent::Async

          def echo(msg)
            msg
          end

          def gather(first, second = nil, *rest)
            [first, second, rest]
          end

          def keywords(a:, b: 2, **extra)
            [a, b, extra]
          end

          def only_required_keyword(a:)
            a
          end

          def positional_only(hash)
            hash
          end

          def splat_only(*args)
            args
          end

          def keyrest_only(**opts)
            opts
          end

          def with_block(&block)
            block.call
          end

          def without_block(value)
            value
          end

          def boom(ex = StandardError.new('boom'))
            @boom_runs = (@boom_runs || 0) + 1
            raise ex
          end

          attr_reader :boom_runs

          private

          def secret(value)
            value
          end

          def respond_to_missing?(name, include_private = false)
            name == :dynamic || super
          end

          def method_missing(name, *args, **kwargs, &block)
            [name, args, kwargs]
          end
        end
      end

      let(:object) { validated_class.new }

      describe 'SignatureError' do
        it 'is an ArgumentError so legacy rescue clauses still match' do
          expect(Concurrent::Async::SignatureError.ancestors).to include(ArgumentError)
        end
      end

      describe 'SignatureChecker' do
        let(:checker) { Concurrent::Async::SignatureChecker }

        def status_for(target, method_name, *args, **kwargs, &block)
          checker.check(target, method_name, args, kwargs, !block.nil?).first
        end

        it 'accepts a call matching a fixed-arity public method' do
          expect(status_for(object, :echo, :hi)).to eq checker::ACCEPTED
        end

        it 'rejects too few and too many positional arguments' do
          expect(status_for(object, :echo)).to eq checker::REJECTED
          expect(status_for(object, :echo, 1, 2)).to eq checker::REJECTED
        end

        it 'keeps the minimum arity for methods that also splat' do
          expect(status_for(object, :gather)).to eq checker::REJECTED
          expect(status_for(object, :gather, 1)).to eq checker::ACCEPTED
          expect(status_for(object, :gather, 1, 2, 3, 4)).to eq checker::ACCEPTED
        end

        it 'accepts any number of arguments for a pure splat method' do
          expect(status_for(object, :splat_only)).to eq checker::ACCEPTED
          expect(status_for(object, :splat_only, 1, 2, 3)).to eq checker::ACCEPTED
        end

        it 'interprets keyword arguments against keyword parameters' do
          expect(status_for(object, :keywords, a: 1)).to eq checker::ACCEPTED
          expect(status_for(object, :keywords, a: 1, b: 3, c: 4)).to eq checker::ACCEPTED
          expect(status_for(object, :only_required_keyword)).to eq checker::REJECTED
          expect(status_for(object, :only_required_keyword, a: 1, z: 2)).to eq checker::REJECTED
        end

        it 'accepts arbitrary keywords for a keyrest method' do
          expect(status_for(object, :keyrest_only, x: 1, y: 2)).to eq checker::ACCEPTED
          expect(status_for(object, :keyrest_only)).to eq checker::ACCEPTED
          expect(status_for(object, :keyrest_only, 1)).to eq checker::REJECTED
        end

        it 'treats a positional hash as a positional argument' do
          expect(status_for(object, :positional_only, x: 1)).to eq checker::ACCEPTED
          expect(status_for(object, :positional_only, {x: 1})).to eq checker::ACCEPTED
        end

        it 'rejects a positional hash handed to a keyword-only method' do
          expect(status_for(object, :only_required_keyword, {a: 1})).to eq checker::REJECTED
        end

        it 'validates block arguments according to actual call semantics' do
          # A declared block parameter receives the block normally.
          expect(status_for(object, :with_block) { 1 }).to eq checker::ACCEPTED
          # Ruby silently accepts (and ignores) a block on a method without a
          # block parameter, so the static check cannot reject it without
          # running the method.
          expect(status_for(object, :without_block, 1) { 2 }).to eq checker::ACCEPTED
          expect(status_for(object, :without_block, 1)).to eq checker::ACCEPTED
        end

        it 'leaves a missing-block yield failure to execution, not submission' do
          yielder = Class.new do
            include Concurrent::Async
            def run
              yield
            end
          end.new
          # No block supplied: binding succeeds statically; the LocalJumpError
          # happens only when the method actually runs.
          expect(yielder.async(validate: true).signature_status(:run)).to eq checker::ACCEPTED
          ivar = yielder.await(validate: true).run
          expect(ivar).to be_rejected
          expect(ivar.reason).to be_a LocalJumpError
        end

        it 'rejects missing and private methods' do
          expect(status_for(object, :not_there)).to eq checker::REJECTED
          expect(status_for(object, :secret, 1)).to eq checker::REJECTED
        end

        it 'marks dynamic method_missing targets as unknown' do
          expect(status_for(object, :dynamic, 1, x: 2)).to eq checker::UNKNOWN
        end

        it 'still validates concrete methods on objects that support method_missing' do
          expect(status_for(object, :echo, 1)).to eq checker::ACCEPTED
          expect(status_for(object, :echo)).to eq checker::REJECTED
        end
      end

      shared_examples 'signature validated proxy' do |proxy_factory|
        it 'runs an accepted call through the normal IVar protocol' do
          ivar = proxy_factory.call(object, :echo, [:foo])
          expect(ivar).to be_a Concurrent::IVar
          ivar.wait
          expect(ivar.value).to eq :foo
          expect(ivar).to be_fulfilled
        end

        it 'forwards keyword arguments and positional hashes distinctly' do
          ivar = proxy_factory.call(object, :keywords, [], a: 1, b: 3, c: 4)
          ivar.wait
          expect(ivar.value).to eq [1, 3, {c: 4}]

          ivar = proxy_factory.call(object, :positional_only, [{x: 9}])
          ivar.wait
          expect(ivar.value).to eq({x: 9})
        end

        it 'folds keyword arguments into a positional hash for non-keyword methods' do
          ivar = proxy_factory.call(object, :positional_only, [], x: 9)
          ivar.wait
          expect(ivar.value).to eq({x: 9})
        end

        it 'raises SignatureError synchronously for an arity mismatch' do
          expect { proxy_factory.call(object, :gather, []) }.
            to raise_error(Concurrent::Async::SignatureError)
          expect { proxy_factory.call(object, :echo, [1, 2]) }.
            to raise_error(Concurrent::Async::SignatureError)
        end

        it 'raises SignatureError for a missing keyword' do
          expect { proxy_factory.call(object, :only_required_keyword, []) }.
            to raise_error(Concurrent::Async::SignatureError, /missing keyword/)
        end

        it 'raises SignatureError for an unknown keyword without keyrest' do
          expect { proxy_factory.call(object, :only_required_keyword, [], a: 1, z: 2) }.
            to raise_error(Concurrent::Async::SignatureError, /unknown keyword/)
        end

        it 'accepts a block even when the method declares no block parameter' do
          # Ruby ignores an unused block; this must be accepted at submission
          # time rather than rejected.
          ivar = proxy_factory.call(object, :without_block, [1]) { :ignored }
          ivar.wait
          expect(ivar.value).to eq 1
        end

        it 'never invokes the business method for a rejected call' do
          before = object.boom_runs.to_i
          expect { proxy_factory.call(object, :boom, [1, 2, 3]) }.
            to raise_error(Concurrent::Async::SignatureError)
          expect(object.boom_runs.to_i).to eq before
        end

        it 'reports an internal exception as a rejected future, not a submission error' do
          error = StandardError.new('inside')
          ivar = proxy_factory.call(object, :boom, [error])
          ivar.wait
          expect(ivar).to be_rejected
          expect(ivar.reason).to be error
          expect(object.boom_runs.to_i).to eq 1
        end

        it 'rejects private methods without exposing them through send' do
          expect { proxy_factory.call(object, :secret, [1]) }.
            to raise_error(Concurrent::Async::SignatureError)
          expect(object.respond_to?(:secret)).to be false
        end

        it 'allows dynamic method_missing calls through as unknown' do
          ivar = proxy_factory.call(object, :dynamic, [1], x: 2)
          ivar.wait
          expect(ivar).to be_fulfilled
          expect(ivar.value).to eq [:dynamic, [1], {x: 2}]
        end

        it 'rejects calls to methods that do not exist' do
          expect { proxy_factory.call(object, :ghost, []) }.
            to raise_error(Concurrent::Async::SignatureError, /undefined method/)
        end

        it 'supports methods that take a block' do
          ivar = proxy_factory.call(object, :with_block, []) { :block_value }
          ivar.wait
          expect(ivar.value).to eq :block_value
        end
      end

      describe '#async with validate: true' do
        include_examples 'signature validated proxy',
                         proc { |obj, method_name, args, kwargs = {}, &block|
                           obj.async(validate: true).send(method_name, *args, **kwargs, &block)
                         }

        it 'returns a pending IVar for an accepted asynchronous call' do
          ivar = object.async(validate: true).echo(:soon)
          expect(ivar).to be_a Concurrent::IVar
          expect(ivar).to be_pending
          expect(ivar.value).to eq :soon
        end

        it 'does not enqueue a rejected call' do
          expect { object.async(validate: true).gather }.
            to raise_error(Concurrent::Async::SignatureError)
          # A subsequent valid call must run immediately rather than queue behind anything.
          ivar = object.await.echo(:clean)
          expect(ivar.value).to eq :clean
        end
      end

      describe '#await with validate: true' do
        include_examples 'signature validated proxy',
                         proc { |obj, method_name, args, kwargs = {}, &block|
                           obj.await(validate: true).send(method_name, *args, **kwargs, &block)
                         }

        it 'returns a fulfilled IVar for an accepted synchronous call' do
          ivar = object.await(validate: true).echo(:now)
          expect(ivar).to be_a Concurrent::IVar
          expect(ivar).to be_fulfilled
          expect(ivar.value).to eq :now
        end
      end

      describe 'rule sharing between async and await' do
        it 'rejects the same invalid shape through both proxies' do
          expect { object.async(validate: true).only_required_keyword }.
            to raise_error(Concurrent::Async::SignatureError)
          expect { object.await(validate: true).only_required_keyword }.
            to raise_error(Concurrent::Async::SignatureError)
        end

        it 'classifies a dynamic method as unknown on both proxies' do
          expect(object.async(validate: true).signature_status(:dynamic)).
            to eq Concurrent::Async::SignatureChecker::UNKNOWN
          expect(object.await(validate: true).signature_status(:dynamic)).
            to eq Concurrent::Async::SignatureChecker::UNKNOWN
        end

        it 'serializes validated and non-validated calls in a single order' do
          box = Class.new {
            include Concurrent::Async
            def gather(seconds, first, *rest)
              sleep(seconds)
              (@bucket ||= []).concat([first]).concat(rest)
            end
            attr_reader :bucket
          }.new

          box.async(validate: true).gather(0.3, :a, :b)
          box.await(validate: true).gather(0, :c, :d)
          expect(box.bucket).to eq [:a, :b, :c, :d]
        end
      end

      describe 'default behavior remains unchanged' do
        it 'keeps the legacy arity check when validation is not requested' do
          expect { object.async.gather }.to raise_error(ArgumentError)
          expect { object.await.gather }.to raise_error(ArgumentError)
        end

        it 'treats validate: false exactly like the default proxy' do
          expect(object.async(validate: false)).to eq object.async
          ivar = object.await(validate: false).echo(:x)
          expect(ivar.value).to eq :x
        end

        it 'still executes accepted calls and surfaces internal failures as rejected futures' do
          ivar = object.async.boom(StandardError.new('legacy'))
          ivar.wait
          expect(ivar).to be_rejected
          expect(ivar.reason.message).to eq 'legacy'
        end
      end
    end
  end
end
