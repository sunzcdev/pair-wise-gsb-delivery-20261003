# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

require 'test_helper'
require 'logger'
require 'stringio'

describe OpenTelemetry::Context do
  after { Context.clear }

  let(:foo_key) { Context.create_key('foo') }
  let(:bar_key) { Context.create_key('bar') }
  let(:baz_key) { Context.create_key('baz') }
  let(:new_context) { Context.empty.set_value(foo_key, 'bar') }

  describe '.create_key' do
    it 'returns a Context::Key' do
      key = Context.create_key('testing')
      _(key).must_be_instance_of(Context::Key)
      _(key.name).must_equal('testing')
    end
  end

  describe '.current' do
    it 'defaults to the root context' do
      _(Context.current).must_equal(Context::ROOT)
    end
  end

  describe '.attach' do
    it 'returns a token to be used when detaching' do
      c1_token = Context.attach(new_context)
      _(c1_token).wont_be_nil
    end

    it 'sets the current context' do
      c1 = new_context
      Context.attach(c1)
      _(Context.current).must_equal(c1)
      _(Context.current[foo_key]).must_equal('bar')

      c2 = Context.current.set_value(foo_key, 'c2')
      Context.attach(c2)
      _(Context.current).must_equal(c2)
      _(Context.current[foo_key]).must_equal('c2')

      c3 = Context.current.set_value(foo_key, 'c3')
      Context.attach(c3)
      _(Context.current).must_equal(c3)
      _(Context.current[foo_key]).must_equal('c3')
    end

    it 'allows for attaching the same context multiple times' do
      c1 = new_context
      token0 = Context.attach(c1)
      token1 = Context.attach(Context.current)
      token2 = Context.attach(Context.current)
      token3 = Context.attach(Context.current)

      # A mismatched detach reports an error and does not pop a stack layer,
      # so the subsequent balanced detaches still unwind the whole stack.
      _(Context.detach(token0)).must_equal(false)
      _(Context.current).must_equal(c1)

      Context.detach(token3)
      _(Context.current).must_equal(c1)
      Context.detach(token2)
      _(Context.current).must_equal(c1)
      Context.detach(token1)
      _(Context.current).must_equal(c1)
      Context.detach(token0)
      _(Context.current).must_equal(Context::ROOT)
    end
  end

  describe '.detach' do
    before { @_error_handler = OpenTelemetry.error_handler }
    after { OpenTelemetry.error_handler = @_error_handler }

    it 'restores the context' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        c1_token = Context.attach(new_context)
        _(Context.current).must_equal(new_context)

        Context.detach(c1_token)
        _(Context.current).must_equal(Context::ROOT)

        _(log_stream.string).must_be_empty
      end
    end

    it 'warns mismatched detach calls' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        c1 = new_context
        c1_token = Context.attach(c1)

        c2 = Context.current.set_value(foo_key, 'c2')
        c2_token = Context.attach(c2)

        c3 = Context.current.set_value(foo_key, 'c3')
        c3_token = Context.attach(c3)

        # Detaching c1's token while c2 and c3 are still active is a stack
        # ordering mismatch. The error is reported but the stack is left
        # untouched so the balanced detaches can still run.
        _(Context.detach(c1_token)).must_equal(false)
        _(Context.current).must_equal(c3)

        _(log_stream.string).must_match(/OpenTelemetry error: calls to detach should match corresponding calls to attach/)

        # The balanced detaches still unwind the full stack because the
        # failed call did not remove any layer.
        Context.detach(c3_token)
        _(Context.current).must_equal(c2)
        Context.detach(c2_token)
        _(Context.current).must_equal(c1)
        Context.detach(c1_token)
        _(Context.current).must_equal(Context::ROOT)
      end
    end

    it 'detaches to the previous context' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        c1 = new_context
        c1_token = Context.attach(c1)

        c2 = Context.current.set_value(foo_key, 'c2')
        c2_token = Context.attach(c2)

        c3 = Context.current.set_value(foo_key, 'c3')
        c3_token = Context.attach(c3)

        _(Context.current).must_equal(c3)

        Context.detach(c3_token)
        _(Context.current).must_equal(c2)

        Context.detach(c2_token)
        _(Context.current).must_equal(c1)

        Context.detach(c1_token)
        _(Context.current).must_equal(Context::ROOT)
        _(log_stream.string).must_be_empty
      end
    end

    it 'detaching with a junk token leaves the current context as root' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        _(Context.detach('junk')).must_equal(false)
        _(Context.current).must_equal(Context::ROOT)
        _(log_stream.string).must_match(/OpenTelemetry error: calls to detach should match corresponding calls to attach/)
      end
    end

    it 'detaching with a junk token does not drop an active stack layer' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        c1 = new_context
        token = Context.attach(c1)

        _(Context.detach('junk')).must_equal(false)
        _(Context.current).must_equal(c1)

        # The owning fiber can still finish its own cleanup.
        _(Context.detach(token)).must_equal(true)
        _(Context.current).must_equal(Context::ROOT)

        _(log_stream.string).must_match(/OpenTelemetry error: calls to detach should match corresponding calls to attach/)
      end
    end

    it 'with a raising error handler' do
      OpenTelemetry.error_handler = lambda { |exception: nil, message: nil|
        raise exception, "OpenTelemetry error: #{[message, exception&.message].compact.join(' - ')}"
      }

      _(-> { Context.detach('junk') }).must_raise(OpenTelemetry::Context::DetachError)
    end
  end

  describe '.with_current' do
    it 'handles nested contexts' do
      c1 = new_context
      Context.with_current(c1) do
        _(Context.current).must_equal(c1)
        c2 = Context.current.set_value(bar_key, 'baz')
        Context.with_current(c2) do
          _(Context.current).must_equal(c2)
        end
        _(Context.current).must_equal(c1)
      end
    end

    it 'resets context when an exception is raised' do
      c1 = new_context
      Context.attach(c1)

      _(proc do
        c2 = Context.current.set_value(bar_key, 'baz')
        Context.with_current(c2) do
          raise 'oops'
        end
      end).must_raise(StandardError)

      _(Context.current).must_equal(c1)
    end

    it 'yields the current context to the block' do
      ctx = new_context
      Context.with_current(ctx) do |c|
        _(c).must_equal(ctx)
      end
    end
  end

  describe '.with_value' do
    it 'executes block within new context' do
      orig_ctx = Context.current

      block_called = false

      Context.with_value(foo_key, 'bar') do |_, value|
        _(Context.current.value(foo_key)).must_equal('bar')
        _(value).must_equal('bar')
        block_called = true
      end

      _(Context.current).must_equal(orig_ctx)
      _(block_called).must_equal(true)
    end

    it 'yields the current context and value to the block' do
      Context.with_value(foo_key, 'bar') do |c, v|
        _(v).must_equal('bar')
        _(c.value(foo_key)).must_equal('bar')
      end
    end
  end

  describe '.with_values' do
    it 'executes block within new context' do
      orig_ctx = Context.current

      block_called = false

      Context.with_values(foo_key => 'bar', bar_key => 'baz') do |_, values|
        _(Context.current.value(foo_key)).must_equal('bar')
        _(Context.current.value(bar_key)).must_equal('baz')
        _(values).must_equal({ foo_key => 'bar', bar_key => 'baz' })
        block_called = true
      end

      _(Context.current).must_equal(orig_ctx)
      _(block_called).must_equal(true)
    end

    it 'yields the current context and values to the block' do
      values = { foo_key => 'bar', bar_key => 'baz' }
      Context.with_values(values) do |c, v|
        _(v).must_equal(values)
        _(c.value(foo_key)).must_equal('bar')
        _(c.value(bar_key)).must_equal('baz')
      end
    end
  end

  describe '.value' do
    it 'returns the value from the current context' do
      Context.attach(new_context)
      _(Context.value(foo_key)).must_equal('bar')

      c2 = Context.current.set_value(bar_key, 'baz')
      Context.attach(c2)
      _(Context.value(bar_key)).must_equal('baz')
    end
  end

  describe '.clear' do
    it 'clears the context' do
      Context.attach(new_context)
      _(Context.current).must_equal(new_context)

      Context.clear

      _(Context.current).must_equal(Context::ROOT)
    end
  end

  describe '#value' do
    it 'returns corresponding value for key' do
      ctx = new_context
      _(ctx.value(foo_key)).must_equal('bar')
    end
  end

  describe '#set_value' do
    it 'returns new context with entry' do
      c1 = Context.current
      c2 = c1.set_value(foo_key, 'bar')
      _(c1.value(foo_key)).must_be_nil
      _(c2.value(foo_key)).must_equal('bar')
    end
  end

  describe '#set_values' do
    it 'assigns multiple values' do
      ctx = new_context
      ctx2 = ctx.set_values(bar_key => 'baz', baz_key => 'quux')
      _(ctx2.value(foo_key)).must_equal('bar')
      _(ctx2.value(bar_key)).must_equal('baz')
      _(ctx2.value(baz_key)).must_equal('quux')
    end

    it 'merges new values' do
      ctx = new_context
      ctx2 = ctx.set_values(foo_key => 'foobar', bar_key => 'baz')
      _(ctx2.value(foo_key)).must_equal('foobar')
      _(ctx2.value(bar_key)).must_equal('baz')
    end
  end

  describe 'fibers' do
    it 'is isolated with respect to Fiber-local variable manipulation' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        ctx = new_context
        Context.with_current(ctx) do
          # This is based on code in ActionController::Live#process:
          # https://github.com/rails/rails/blob/ad0105c13f61d145a659004efb928a643104e973/actionpack/lib/action_controller/metal/live.rb#L270
          t1 = Thread.current
          locals = t1.keys.map { |key| [key, t1[key]] }
          Fiber.new do
            t2 = Thread.current
            locals.each { |k, v| t2[k] = v }
            # Manipulate _this fiber's_ context stack.
            Context.attach(new_context)
          ensure
            locals.each { |k, _| t2[k] = nil } # rubocop:disable Style/HashEachMethods --(locals is not a Hash)
          end.resume
        end
        _(log_stream.string).must_be_empty
      end
    end

    it 'is isolated with respect to Fiber-local storage manipulation' do
      skip unless Fiber.current.respond_to?(:storage)
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        ctx = new_context
        Context.with_current(ctx) do
          # This is based on code in ActionController::Live#process, modified to use Fiber-local storage:
          # https://github.com/rails/rails/blob/ad0105c13f61d145a659004efb928a643104e973/actionpack/lib/action_controller/metal/live.rb#L270
          f1 = Fiber.current
          Fiber[:foo] = :bar
          locals = f1.storage
          Fiber.new do
            f2 = Fiber.current
            locals.each { |k, v| f2.storage[k] = v }
            # Manipulate _this fiber's_ context stack.
            Context.attach(new_context)
          ensure
            locals.each_key { |k| f2.storage.delete(k) }
          end.resume
        end
        _(log_stream.string).must_be_empty
      end
    end

    it 'keeps two manually interleaved fibers balanced through correct attach and detach' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        main = Fiber.current
        fiber_a = nil
        fiber_b = nil
        a_finished = false
        b_finished = false

        fiber_a = Fiber.new do
          ctx_a1 = new_context
          token_a1 = Context.attach(ctx_a1)
          _(Context.current).must_equal(ctx_a1)

          # Hand control to fiber B while A's context remains active.
          fiber_b.transfer

          ctx_a2 = Context.current.set_value(bar_key, 'a2')
          token_a2 = Context.attach(ctx_a2)
          _(Context.current).must_equal(ctx_a2)

          fiber_b.transfer

          Context.detach(token_a2)
          _(Context.current).must_equal(ctx_a1)
          Context.detach(token_a1)
          _(Context.current).must_equal(Context::ROOT)

          a_finished = true
          main.transfer
        end

        fiber_b = Fiber.new do
          ctx_b1 = Context.empty.set_value(foo_key, 'b1')
          token_b1 = Context.attach(ctx_b1)
          _(Context.current).must_equal(ctx_b1)

          fiber_a.transfer

          # B only ever sees its own stack, even though A has nested contexts.
          _(Context.current).must_equal(ctx_b1)
          Context.detach(token_b1)
          _(Context.current).must_equal(Context::ROOT)

          b_finished = true
          fiber_a.transfer
        end

        fiber_a.transfer # A attaches ctx_a1, switches to B
        # B attaches ctx_b1, switches back to A
        # A attaches ctx_a2, switches to B
        # B detaches, switches back to A
        # A detaches both and transfers back to main

        _(a_finished).must_equal(true)
        _(b_finished).must_equal(true)
        _(Context.current).must_equal(Context::ROOT)
        _(log_stream.string).must_be_empty
      end
    end

    it 'reports ownership and stack-order mismatches without dropping a layer when fibers cross' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        main = Fiber.current

        tokens = {}
        contexts = {}
        fiber_a = nil
        fiber_b = nil
        a_finished = false
        b_finished = false

        fiber_a = Fiber.new do
          contexts[:a1] = new_context
          tokens[:a1] = Context.attach(contexts[:a1])
          contexts[:a2] = Context.current.set_value(bar_key, 'a2')
          tokens[:a2] = Context.attach(contexts[:a2])

          fiber_b.transfer

          # The failed detach on B must not have removed any of A's layers.
          _(Context.current).must_equal(contexts[:a2])

          Context.detach(tokens[:a2])
          _(Context.current).must_equal(contexts[:a1])
          Context.detach(tokens[:a1])
          _(Context.current).must_equal(Context::ROOT)

          a_finished = true
          main.transfer
        end

        fiber_b = Fiber.new do
          contexts[:b1] = Context.empty.set_value(baz_key, 'b1')
          tokens[:b1] = Context.attach(contexts[:b1])
          _(Context.current).must_equal(contexts[:b1])

          # A task object resumes on the wrong fiber: B tries to detach a
          # token that A's fiber owns. This is an ownership mismatch.
          _(Context.detach(tokens[:a1])).must_equal(false)

          # B's own stack is untouched by the erroneous call.
          _(Context.current).must_equal(contexts[:b1])

          # A stack-order mismatch on B's own fiber: the outer token cannot
          # be detached while B's inner context is still active.
          inner = Context.attach(Context.empty.set_value(bar_key, 'b2'))
          _(Context.detach(tokens[:b1])).must_equal(false)
          _(Context.current[bar_key]).must_equal('b2')

          # Balanced detaches still work after the reported errors.
          Context.detach(inner)
          _(Context.current).must_equal(contexts[:b1])
          Context.detach(tokens[:b1])
          _(Context.current).must_equal(Context::ROOT)

          b_finished = true
          fiber_a.transfer
        end

        fiber_a.transfer # A attaches two contexts and transfers to B
        # B hits the ownership and ordering mismatches, then cleans up and
        # transfers back to A, which completes its own cleanup and returns.

        _(a_finished).must_equal(true)
        _(b_finished).must_equal(true)
        _(Context.current).must_equal(Context::ROOT)

        log = log_stream.string
        _(log).must_match(/different fiber/)
        _(log).must_match(/top of the current fiber context stack/)
        _(log.scan(/calls to detach should match corresponding calls to attach/).size).must_equal(2)
      end
    end
  end

  describe 'threading' do
    it 'is isolated with respect to Fiber-local variable manipulation' do
      OpenTelemetry::TestHelpers.with_test_logger do |log_stream|
        ctx = new_context
        Context.with_current(ctx) do
          # This is based on code in ActionController::Live#process:
          # https://github.com/rails/rails/blob/ad0105c13f61d145a659004efb928a643104e973/actionpack/lib/action_controller/metal/live.rb#L270
          t1 = Thread.current
          locals = t1.keys.map { |key| [key, t1[key]] }
          Thread.new do
            t2 = Thread.current
            locals.each { |k, v| t2[k] = v }
            # Manipulate _this thread's_ context stack.
            Context.attach(new_context)
          ensure
            locals.each { |k, _| t2[k] = nil } # rubocop:disable Style/HashEachMethods --(locals is not a Hash)
          end.join
        end
        _(log_stream.string).must_be_empty
      end
    end

    it 'unwinds the stack on each thread' do
      ctx = new_context
      t1_ctx_before = Context.current
      Context.with_current(ctx) do
        Thread.new do
          t2_ctx_before = Context.current
          Context.with_current(ctx) do
            Context.with_value(bar_key, 'foobar') do
              _(Context.current).wont_equal(t2_ctx_before)
            end
          end
          _(Context.current).must_equal(t2_ctx_before)
        end.join
        Context.with_value(bar_key, 'baz') do
          _(Context.current).wont_equal(t1_ctx_before)
        end
      end
      _(Context.current).must_equal(t1_ctx_before)
    end

    it 'scopes changes to the current thread' do
      ctx = new_context
      Context.with_current(ctx) do
        Thread.new do
          Context.with_current(ctx) do
            Context.with_value(bar_key, 'foobar') do
              Thread.pass
              _(Context.current[foo_key]).must_equal('bar')
              _(Context.current[bar_key]).must_equal('foobar')
            end
            _(Context.current[bar_key]).must_be_nil
          end
        end.join
        Context.with_value(bar_key, 'baz') do
          Thread.pass
          _(Context.current[foo_key]).must_equal('bar')
          _(Context.current[bar_key]).must_equal('baz')
        end
        _(Context.current[bar_key]).must_be_nil
      end
    end
  end
end

