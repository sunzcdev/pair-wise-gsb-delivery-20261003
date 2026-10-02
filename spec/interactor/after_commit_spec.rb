describe "after_commit" do
  def build_interactor(&block)
    interactor = Class.new.send(:include, Interactor)
    interactor.class_eval(&block) if block
    interactor
  end

  def build_organizer(options = {}, &block)
    organizer = Class.new.send(:include, Interactor::Organizer)
    organizer.organize(options[:organize]) if options[:organize]
    organizer.class_eval(&block) if block
    organizer
  end

  let(:events) { [] }
  let(:context) { Interactor::Context.new(events: events) }

  context "with a single interactor" do
    let(:interactor) {
      build_interactor do
        def call
          context.events << :call
          after_commit { context.events << :notify1 }
          context.events << :call_continues
          after_commit { context.events << :notify2 }
        end

        def rollback
          context.events << :rollback
        end
      end
    }

    it "runs callbacks after the call completes, in registration order" do
      interactor.call(context)

      expect(events).to eq([:call, :call_continues, :notify1, :notify2])
    end

    it "forms its own commit boundary for each direct call" do
      interactor.call(events: events)
      interactor.call(events: events)

      expect(events).to eq([
        :call, :call_continues, :notify1, :notify2,
        :call, :call_continues, :notify1, :notify2
      ])
    end

    context "when the call fails" do
      let(:interactor) {
        build_interactor do
          def call
            context.events << :call
            after_commit { context.events << :notify }
            context.fail!
          end

          def rollback
            context.events << :rollback
          end
        end
      }

      it "discards the registered callbacks" do
        interactor.call(context)

        expect(events).to eq([:call])
      end

      it "leaves nothing pending for the next call" do
        interactor.call(events: events)
        expect(events).to eq([:call])

        events.clear
        interactor.call(events: events)
        expect(events).to eq([:call])
      end
    end
  end

  context "with a nested organizer" do
    let(:interactor1) {
      build_interactor do
        def call
          context.events << :call1
          after_commit { context.events << :notify1 }
        end

        def rollback
          context.events << :rollback1
        end
      end
    }

    let(:interactor2) {
      build_interactor do
        def call
          context.events << :call2
          after_commit { context.events << :notify2 }
        end

        def rollback
          context.events << :rollback2
        end
      end
    }

    let(:organizer) {
      inner = build_organizer(organize: [interactor1, interactor2])
      build_organizer(organize: [inner])
    }

    it "commits nested registrations once the outermost call succeeds" do
      organizer.call(context)

      expect(events).to eq([:call1, :call2, :notify1, :notify2])
    end

    context "when a later step fails" do
      let(:interactor2) {
        build_interactor do
          def call
            context.events << :call2
            after_commit { context.events << :notify2 }
            context.fail!
          end

          def rollback
            context.events << :rollback2
          end
        end
      }

      it "rolls back and discards the whole batch" do
        result = organizer.call(context)

        expect(result).to be_failure
        expect(events).to eq([:call1, :call2, :rollback1])
      end

      it "leaves no notifications pending for a later call" do
        organizer.call(context)
        expect(events).to eq([:call1, :call2, :rollback1])

        events.clear
        organizer.call(events: events)
        expect(events).to eq([:call1, :call2, :rollback1])
      end
    end
  end

  context "when a callback starts a brand-new business call" do
    let(:inner) {
      build_interactor do
        def call
          context.events << :inner_call
          after_commit { context.events << :inner_notify }
        end
      end
    }

    let(:outer) {
      inner_interactor = inner
      build_interactor do
        define_method(:call) do
          context.events << :outer_call
          after_commit { context.events << :outer_notify1 }
          after_commit do
            context.events << :kickoff
            inner_interactor.call(events: context.events)
          end
          after_commit { context.events << :outer_notify2 }
        end
      end
    }

    it "gives the new call its own independent commit boundary" do
      outer.call(context)

      expect(events).to eq([
        :outer_call,
        :outer_notify1,
        :kickoff, :inner_call, :inner_notify,
        :outer_notify2
      ])
    end
  end

  context "when a callback fails" do
    let(:interactor) {
      build_interactor do
        def call
          context.events << :call
          after_commit { context.events << :notify1 }
          after_commit { raise "boom1" }
          after_commit { context.events << :notify2 }
          after_commit { raise "boom2" }
        end

        def rollback
          context.events << :rollback
        end
      end
    }

    it "does not roll back the business call" do
      expect { interactor.call!(context) }.to raise_error(Interactor::AfterCommitError)

      expect(events).to eq([:call, :notify1, :notify2])
    end

    it "still runs the remaining registered callbacks" do
      begin
        interactor.call!(context)
      rescue Interactor::AfterCommitError
        nil
      end

      expect(events).to include(:notify1, :notify2)
    end

    it "raises an aggregate error exposing every callback failure" do
      expect { interactor.call!(context) }.to raise_error(Interactor::AfterCommitError) { |error|
        expect(error.errors.map(&:message)).to eq(["boom1", "boom2"])
      }
    end

    it "leaves nothing pending for the next call" do
      begin
        interactor.call!(context)
      rescue Interactor::AfterCommitError
        nil
      end
      expect(context._after_commit_callbacks).to be_empty
    end
  end
end
