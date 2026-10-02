module Interactor
  describe "after_commit" do
    # A shared, in-memory sink standing in for an email/notification service.
    let(:sink) { {notifications: [], events: []} }

    def actor(name, sink, &block)
      klass = Class.new.send(:include, Interactor)
      klass.define_singleton_method(:name) { name }
      klass.send(:define_method, :call) do
        sink[:events] << :"call_#{name}"
        instance_exec(&block) if block
      end
      klass.send(:define_method, :rollback) do
        sink[:events] << :"rollback_#{name}"
      end
      klass
    end

    def chain(name, organized)
      klass = Class.new.send(:include, Interactor::Organizer)
      klass.define_singleton_method(:name) { name }
      klass.organize(organized)
      klass
    end

    describe "registration from #call" do
      it "runs callbacks only after the interactor succeeds" do
        sink = self.sink
        klass = actor("one", sink) do
          after_commit { sink[:notifications] << :one }
        end

        context = klass.call

        expect(context).to be_success
        expect(sink[:events]).to eq([:call_one])
        expect(sink[:notifications]).to eq([:one])
      end

      it "establishes its own commit boundary for a single interactor" do
        sink = self.sink
        klass = Class.new.send(:include, Interactor)
        klass.send(:define_method, :call) do
          sink[:events] << :business
          after_commit { sink[:events] << :commit }
        end

        klass.call

        expect(sink[:events]).to eq([:business, :commit])
      end

      it "runs callbacks in registration order" do
        sink = self.sink
        klass = Class.new.send(:include, Interactor)
        klass.send(:define_method, :call) do
          after_commit { sink[:notifications] << :a }
          after_commit { sink[:notifications] << :b }
          after_commit { sink[:notifications] << :c }
        end

        klass.call

        expect(sink[:notifications]).to eq([:a, :b, :c])
      end
    end

    describe "class-level after_commit hook" do
      it "supports a block hook" do
        sink = self.sink
        klass = actor("hooked", sink)
        klass.after_commit { sink[:notifications] << :class_block }

        klass.call

        expect(sink[:notifications]).to eq([:class_block])
      end

      it "supports a symbol method hook" do
        sink = self.sink
        klass = actor("symbol", sink)
        klass.after_commit(:send_email)
        klass.send(:define_method, :send_email) do
          sink[:notifications] << :symbol_email
        end
        klass.send(:private, :send_email)

        klass.call

        expect(sink[:notifications]).to eq([:symbol_email])
      end

      it "runs instance-registered callbacks before the class hook, in order" do
        sink = self.sink
        klass = actor("mixed", sink) do
          after_commit { sink[:notifications] << :instance_hook }
        end
        klass.after_commit { sink[:notifications] << :class_hook }

        klass.call

        expect(sink[:notifications]).to eq([:instance_hook, :class_hook])
      end
    end

    describe "nested organizers" do
      let(:one) {
        sink = self.sink
        actor("one", sink) do
          after_commit { sink[:notifications] << :one }
        end
      }
      let(:two) {
        sink = self.sink
        actor("two", sink) do
          after_commit { sink[:notifications] << :two }
        end
      }
      let(:three) {
        sink = self.sink
        actor("three", sink) do
          after_commit { sink[:notifications] << :three }
        end
      }
      let(:inner) { chain("inner", [one, two]) }
      let(:outer) { chain("outer", [inner, three]) }

      it "defers every callback until the outermost call succeeds" do
        sink = self.sink
        outer.call

        expect(sink[:events]).to eq([:call_one, :call_two, :call_three])
        expect(sink[:notifications]).to eq([:one, :two, :three])
      end

      it "runs no commit callbacks and rolls prior steps back on failure" do
        sink = self.sink
        failing = actor("failing", sink) do
          after_commit { sink[:notifications] << :should_never_send }
          context.fail!
        end
        bad_chain = chain("chain", [one, two, failing, three])

        context = bad_chain.call

        expect(context).to be_failure
        expect(sink[:events]).to eq([
          :call_one,
          :call_two,
          :call_failing,
          :rollback_two,
          :rollback_one
        ])
        expect(sink[:notifications]).to eq([])
      end

      it "runs no commit callbacks and rolls prior steps back on a raise" do
        sink = self.sink
        raising = actor("raising", sink) do
          after_commit { sink[:notifications] << :should_never_send }
          raise "boom"
        end
        bad_chain = chain("chain", [one, raising])

        expect { bad_chain.call }.to raise_error("boom")
        expect(sink[:events]).to eq([:call_one, :call_raising, :rollback_one])
        expect(sink[:notifications]).to eq([])
      end
    end

    describe "a business call started during a commit callback" do
      it "opens an independent boundary instead of joining the draining batch" do
        sink = self.sink
        inner = Class.new.send(:include, Interactor)
        inner.define_singleton_method(:name) { "inner" }
        inner.send(:define_method, :call) do
          sink[:events] << :inner_business
          after_commit { sink[:events] << :inner_commit }
        end

        outer = Class.new.send(:include, Interactor)
        outer.define_singleton_method(:name) { "outer" }
        outer.send(:define_method, :call) do
          sink[:events] << :outer_business
          after_commit do
            sink[:events] << :outer_commit_start
            inner.call
            sink[:events] << :outer_commit_end
          end
        end

        context = outer.call

        expect(context).to be_success
        expect(sink[:events]).to eq([
          :outer_business,
          :outer_commit_start,
          :inner_business,
          :inner_commit,
          :outer_commit_end
        ])
      end

      it "isolates a nested callback failure to the nested boundary" do
        sink = self.sink
        failing_inner = Class.new.send(:include, Interactor)
        failing_inner.define_singleton_method(:name) { "failing_inner" }
        failing_inner.send(:define_method, :call) do
          sink[:events] << :failing_inner_business
          after_commit { raise "inner callback boom" }
        end

        outer = Class.new.send(:include, Interactor)
        outer.define_singleton_method(:name) { "outer" }
        outer.send(:define_method, :call) do
          sink[:events] << :outer_business
          after_commit do
            sink[:events] << :outer_commit
            begin
              failing_inner.call!
            rescue CommitError => error
              sink[:events] << :"caught_#{error.result.failures.length}"
            end
          end
        end

        context = outer.call

        expect(context).to be_success
        expect(context.after_commit_result).to be_success
        expect(sink[:events]).to eq([
          :outer_business,
          :outer_commit,
          :failing_inner_business,
          :"caught_1"
        ])
      end
    end

    describe "commit failures" do
      it "keeps running the remaining callbacks and aggregates the failures" do
        sink = self.sink
        good1 = actor("good1", sink) do
          after_commit { sink[:notifications] << :good1 }
        end
        bad = actor("bad", sink) do
          after_commit { raise "callback boom" }
        end
        good2 = actor("good2", sink) do
          after_commit { sink[:notifications] << :good2 }
        end
        organizer = chain("chain", [good1, bad, good2])

        context = organizer.call

        expect(sink[:notifications]).to eq([:good1, :good2])
        expect(context).to be_success
        result = context.after_commit_result
        expect(result).to be_failure
        expect(result.callbacks.length).to eq(3)
        expect(result.succeeded.length).to eq(2)
        expect(result.failures.length).to eq(1)
        expect(result.exceptions.map(&:message)).to eq(["callback boom"])
      end

      it "does not trigger a business rollback after a callback failure" do
        sink = self.sink
        bad = actor("bad", sink) do
          after_commit { raise "callback boom" }
        end

        context = bad.call

        expect(sink[:events]).to eq([:call_bad])
        expect(context).to be_success
      end

      it "raises an inspectable CommitError from call!" do
        sink = self.sink
        bad = actor("bad", sink) do
          after_commit { raise "callback boom" }
        end

        expect {
          bad.call!
        }.to raise_error(CommitError) { |error|
          expect(error.result).to be_failure
          expect(error.result.exceptions.map(&:message)).to eq(["callback boom"])
        }
      end
    end

    describe "state between calls" do
      it "leaves no callbacks behind after a failure to leak into a later call" do
        sink = self.sink
        good = actor("good", sink) do
          after_commit { sink[:notifications] << :good }
        end
        failing = actor("failing", sink) do
          after_commit { sink[:notifications] << :never }
          context.fail!
        end

        failing.call
        expect(sink[:notifications]).to eq([])

        good.call
        expect(sink[:notifications]).to eq([:good])
      end
    end

    describe "registration outside of an invocation" do
      it "raises a CommitRegistrationError" do
        sink = self.sink
        context = Context.new

        expect {
          context.after_commit { sink[:notifications] << :late }
        }.to raise_error(CommitRegistrationError)
      end
    end
  end
end
