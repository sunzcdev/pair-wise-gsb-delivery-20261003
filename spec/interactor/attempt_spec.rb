describe "Context#attempt" do
  # Registry of every completed step instance, keyed by a unique id captured at
  # call time. Lets the examples assert that a given instance lands in exactly
  # one rollback position even when the same class is used repeatedly.
  class Ledger
    attr_reader :events

    def initialize
      @events = []
      @seq = 0
    end

    def reset
      @events = []
      @seq = 0
    end

    def call!(name, context, on_call: nil, failure: nil, raises: nil)
      @seq += 1
      id = @seq
      @events << "call:#{id}:#{name}"
      on_call&.call(context)
      raise raises if raises
      context.fail!(failure) if failure
      id
    end

    def rollback!(name, id)
      @events << "rollback:#{id}:#{name}"
    end

    def call_ids
      @events.select { |e| e.start_with?("call:") }
        .map { |e| e.split(":")[1].to_i }
    end

    def rollback_ids
      @events.select { |e| e.start_with?("rollback:") }
        .map { |e| e.split(":")[1].to_i }
    end

    def rollback_names
      @events.select { |e| e.start_with?("rollback:") }
        .map { |e| e.split(":")[2] }
    end

    def call_names
      @events.select { |e| e.start_with?("call:") }
        .map { |e| e.split(":")[2] }
    end
  end

  def build_interactor(&block)
    Class.new.send(:include, Interactor).tap do |interactor|
      interactor.class_eval(&block) if block
    end
  end

  def build_organizer(&block)
    Class.new.send(:include, Interactor::Organizer).tap do |organizer|
      organizer.class_eval(&block) if block
    end
  end

  # A step that records itself in the shared ledger. Returns the interactor
  # class; each invocation produces a fresh tracked instance with its own id.
  def build_step(name, failure: nil, raises: nil, on_call: nil)
    ledger = $ledger
    build_interactor do
      define_method(:call) do
        @step_id = ledger.call!(
          name, context, on_call: on_call, failure: failure, raises: raises
        )
      end

      define_method(:rollback) do
        ledger.rollback!(name, @step_id)
      end
    end
  end

  before { $ledger = Ledger.new }

  # Asserts every rollback id is unique and refers to an instance that was
  # called, i.e. each completed instance occupies exactly one rollback slot.
  def assert_exactly_one_rollback_each
    ids = $ledger.rollback_ids
    expect(ids).to eq(ids.uniq)
    expect(ids - $ledger.call_ids).to be_empty
  end

  describe "success" do
    it "merges the sub-flow output and records into the outer flow" do
      main = build_step("main")
      supplement = build_step("supplement",
        on_call: ->(c) { c.supplement = "done" })

      organizer = build_organizer do
        define_method(:call) do
          context.main = "main"
          main.call!(context)
          context.attempt(supplement)
        end
      end

      result = organizer.call

      expect(result).to be_success
      expect(result.main).to eq("main")
      expect(result.supplement).to eq("done")
      expect($ledger.call_names).to eq(%w[main supplement])
      expect($ledger.rollback_names).to be_empty
    end

    it "still rolls sub-flow steps back when the outer flow later fails" do
      main = build_step("main")
      supplement = build_step("supplement")
      after_step = build_step("after")
      failing = build_step("failing", failure: { error: "outer" })

      organizer = build_organizer do
        define_method(:call) do
          main.call!(context)
          context.attempt(supplement)
          after_step.call!(context)
          failing.call!(context)
        end
      end

      result = organizer.call

      expect(result).to be_failure
      expect(result.error).to eq("outer")
      # The instance that called fail! is not itself rolled back; every
      # successfully completed step before it is, in reverse order, including
      # the ones that came from the successful sub-flow.
      expect($ledger.call_names).to eq(
        %w[main supplement after failing]
      )
      expect($ledger.rollback_names).to eq(
        %w[after supplement main]
      )
      assert_exactly_one_rollback_each
    end
  end

  describe "business failure" do
    let(:main) { build_step("main") }

    it "rolls back sub-flow steps, restores top-level keys, and resumes" do
      done = build_step("done")
      failing = build_step("failing",
        on_call: ->(c) {
          c.added = "new"
          c.overwritten = "changed"
          c.delete_field(:preserved)
        },
        failure: { error: "supplement failed" })

      main_step = main
      sub = build_organizer do
        define_method(:call) do
          done.call!(context)
          failing.call!(context)
        end
      end
      organizer = build_organizer do
        define_method(:call) do
          context.overwritten = "original"
          context.preserved = "value"
          main_step.call!(context)
          context.attempt(sub) do |failure|
            context.reason = failure.error
          end
          context.recovered = true
        end
      end

      result = organizer.call

      expect(result).to be_success
      expect(result.recovered).to be(true)
      expect(result.reason).to eq("supplement failed")

      expect(result.to_h).not_to include(:added)
      expect(result.overwritten).to eq("original")
      expect(result.preserved).to eq("value")

      expect($ledger.call_names).to eq(%w[main done failing])
      # "done" completed inside the sub-flow and rolls back; "failing" is the
      # failing instance itself and is not rolled back; "main" survives.
      expect($ledger.rollback_names).to eq(%w[done])
      assert_exactly_one_rollback_each

      surviving = result._called
        .map { |i| i.instance_variable_get(:@step_id) }
        .compact
      expect(surviving.length).to eq(1)
      expect($ledger.rollback_ids & surviving).to be_empty
    end

    it "passes the recovery block a failed snapshot holding the reason" do
      captured = nil
      failing = build_step("failing",
        on_call: ->(c) { c.added = "new" },
        failure: { error: "boom" })

      organizer = build_organizer do
        define_method(:call) do
          context.attempt(failing) { |failure| captured = failure }
        end
      end

      organizer.call

      expect(captured).to be_a(Interactor::Context)
      expect(captured).to be_failure
      expect(captured.error).to eq("boom")
      expect(captured.added).to eq("new")
    end

    it "resumes without a recovery block" do
      done = build_step("done")
      failing = build_step("failing", failure: { error: "x" })
      sub = build_organizer do
        define_method(:call) do
          done.call!(context)
          failing.call!(context)
        end
      end

      main_step = main
      organizer = build_organizer do
        define_method(:call) do
          main_step.call!(context)
          context.attempt(sub)
          context.kept_going = true
        end
      end

      result = organizer.call

      expect(result).to be_success
      expect(result.kept_going).to be(true)
      expect($ledger.rollback_names).to eq(%w[done])
      assert_exactly_one_rollback_each
    end
  end

  describe "nested two levels" do
    # outer
    #  └─ attempt(inner)
    #      ├─ inner_ok
    #      └─ attempt(deep)
    #          ├─ deep_done
    #          └─ deep_fail (fail!, recovered inside deep)
    # after recovering, inner succeeds; outer continues.
    it "rolls back only steps completed inside the deepest attempt" do
      outer_ok = build_step("outer_ok")
      inner_ok = build_step("inner_ok")
      deep_ok = build_step("deep_ok")
      deep_done = build_step("deep_done")
      deep_fail = build_step("deep_fail", failure: { error: "deep" })

      deep_sub = build_organizer do
        define_method(:call) do
          deep_done.call!(context)
          deep_fail.call!(context)
        end
      end

      deep = build_organizer do
        define_method(:call) do
          deep_ok.call!(context)
          context.attempt(deep_sub) do |failure|
            $ledger.events << "recovery:#{failure.error}"
          end
        end
      end

      inner = build_organizer do
        define_method(:call) do
          inner_ok.call!(context)
          context.attempt(deep)
          context.inner_finished = true
        end
      end

      outer = build_organizer do
        define_method(:call) do
          outer_ok.call!(context)
          context.attempt(inner)
          context.outer_finished = true
        end
      end

      result = outer.call

      expect(result).to be_success
      expect(result.inner_finished).to be(true)
      expect(result.outer_finished).to be(true)
      expect($ledger.call_names).to eq(
        %w[outer_ok inner_ok deep_ok deep_done deep_fail]
      )
      # deep_done ran inside the recovered attempt and rolls back; deep_ok ran
      # in deep itself (outside that attempt) and survives.
      expect($ledger.rollback_names).to eq(%w[deep_done])
      expect($ledger.events.grep(/^recovery:/)).to eq(["recovery:deep"])
      assert_exactly_one_rollback_each

      tracked_ids = result._called
        .map { |i| i.instance_variable_get(:@step_id) }
        .compact
      expect(tracked_ids.length).to eq(3)
      expect($ledger.rollback_ids & tracked_ids).to be_empty
    end

    # Outer attempt's sub-flow fails only after an inner attempt has already
    # successfully recovered. The inner attempt's surviving steps must roll
    # back together with the inner flow, in reverse order.
    it "unwinds recovered inner steps when the outer sub-flow then fails" do
      outer_ok = build_step("outer_ok")
      inner_a = build_step("inner_a")
      deep_ok = build_step("deep_ok")
      deep_fail = build_step("deep_fail", failure: { error: "deep" })
      inner_last = build_step("inner_last")
      inner_crash = build_step("inner_crash", failure: { error: "inner" })

      deep = build_organizer do
        define_method(:call) do
          deep_ok.call!(context)
          context.attempt(deep_fail)
        end
      end

      inner = build_organizer do
        define_method(:call) do
          inner_a.call!(context)
          context.attempt(deep)
          inner_last.call!(context)
          inner_crash.call!(context)
        end
      end

      outer = build_organizer do
        define_method(:call) do
          outer_ok.call!(context)
          context.attempt(inner) do |failure|
            $ledger.events << "recovery:#{failure.error}"
          end
        end
      end

      result = outer.call

      expect(result).to be_success
      expect($ledger.call_names).to eq(
        %w[outer_ok inner_a deep_ok deep_fail inner_last inner_crash]
      )
      expect($ledger.rollback_names).to eq(
        %w[inner_last deep_ok inner_a]
      )
      expect($ledger.events.grep(/^recovery:/)).to eq(["recovery:inner"])
      assert_exactly_one_rollback_each

      tracked_ids = result._called
        .map { |i| i.instance_variable_get(:@step_id) }
        .compact
      expect(tracked_ids.length).to eq(1)
      expect($ledger.rollback_ids & tracked_ids).to be_empty
    end
  end

  describe "the same interactor class repeated" do
    it "tracks each instance separately and rolls each back at most once" do
      step = build_step("step")
      done = build_step("done")
      flaky = build_step("flaky", failure: { error: "no" })

      sub = build_organizer do
        define_method(:call) do
          done.call!(context)
          flaky.call!(context)
        end
      end

      organizer = build_organizer do
        define_method(:call) do
          step.call!(context)
          context.attempt(sub)
          step.call!(context)
          context.attempt(sub)
          step.call!(context)
        end
      end

      result = organizer.call

      expect(result).to be_success
      expect($ledger.call_names).to eq(
        %w[step done flaky step done flaky step]
      )
      expect($ledger.rollback_names).to eq(%w[done done])
      assert_exactly_one_rollback_each

      # The two "done" rollbacks belong to distinct instances.
      done_call_ids = $ledger.events
        .select { |e| e.start_with?("call:") && e.end_with?(":done") }
        .map { |e| e.split(":")[1].to_i }
      expect(done_call_ids.uniq.length).to eq(2)
      expect($ledger.rollback_ids).to match_array(done_call_ids)

      # The three successful "step" instances survive and never roll back.
      step_call_ids = $ledger.events
        .select { |e| e.start_with?("call:") && e.end_with?(":step") }
        .map { |e| e.split(":")[1].to_i }
      expect(step_call_ids.uniq.length).to eq(3)
      expect($ledger.rollback_ids & step_call_ids).to be_empty
    end
  end

  describe "recovery block failure" do
    it "unwinds the sub-flow and is caught by an enclosing attempt" do
      outer_ok = build_step("outer_ok")
      inner_ok = build_step("inner_ok")
      failing = build_step("failing", failure: { error: "inner" })

      inner = build_organizer do
        define_method(:call) do
          inner_ok.call!(context)
          context.attempt(failing) do
            $ledger.events << "recovery:inner"
            context.fail!(error: "recovery failed")
          end
        end
      end

      outer = build_organizer do
        define_method(:call) do
          outer_ok.call!(context)
          context.attempt(inner) do |failure|
            $ledger.events << "recovery:outer:#{failure.error}"
          end
          context.outer_finished = true
        end
      end

      result = outer.call

      expect(result).to be_success
      expect(result.outer_finished).to be(true)
      expect($ledger.events.grep(/^recovery:/)).to eq(
        ["recovery:inner", "recovery:outer:recovery failed"]
      )
      expect($ledger.call_names).to eq(
        %w[outer_ok inner_ok failing]
      )
      # "failing" itself is not rolled back; inner_ok rolls back as the failed
      # recovery unwinds the inner sub-flow; outer_ok is untouched.
      expect($ledger.rollback_names).to eq(%w[inner_ok])
      assert_exactly_one_rollback_each

      tracked_ids = result._called
        .map { |i| i.instance_variable_get(:@step_id) }
        .compact
      expect(tracked_ids.length).to eq(1)
      expect($ledger.rollback_ids & tracked_ids).to be_empty
    end

    it "fails and fully rolls back the outer flow with no enclosing attempt" do
      main = build_step("main")
      done = build_step("done")
      failing = build_step("failing", failure: { error: "inner" })

      sub = build_organizer do
        define_method(:call) do
          done.call!(context)
          failing.call!(context)
        end
      end

      organizer = build_organizer do
        define_method(:call) do
          main.call!(context)
          context.attempt(sub) do
            context.fail!(error: "recovery failed")
          end
        end
      end

      result = organizer.call

      expect(result).to be_failure
      expect(result.error).to eq("recovery failed")
      expect($ledger.rollback_names).to eq(%w[done main])
      assert_exactly_one_rollback_each
      tracked_ids = result._called
        .map { |i| i.instance_variable_get(:@step_id) }
        .compact
      expect(tracked_ids).to be_empty
    end
  end

  describe "ordinary exceptions" do
    it "does not swallow a failure raised against another context" do
      other_context = Interactor::Context.new

      # A plain interactor (not an organizer) whose run! raises a Failure tied
      # to a different context. "attempt" must only contain failures of its
      # own context.
      sub = Class.new.send(:include, Interactor)
      sub.class_eval do
        define_method(:call) do
          other_context.fail!(error: "foreign")
        end
      end

      organizer = build_organizer do
        define_method(:call) do
          context.attempt(sub) { |_| raise "recovery must not run" }
        end
      end

      expect {
        organizer.call!
      }.to raise_error(Interactor::Failure) { |error|
        expect(error.context).to equal(other_context)
      }

      expect($ledger.events.grep(/^recovery:/)).to be_empty
    end

    it "propagates after rolling back sub-flow and outer steps" do
      main = build_step("main")
      done = build_step("done")
      bomb = build_step("bomb", raises: RuntimeError.new("kaboom"))

      sub = build_organizer do
        define_method(:call) do
          done.call!(context)
          bomb.call!(context)
        end
      end

      organizer = build_organizer do
        define_method(:call) do
          main.call!(context)
          context.attempt(sub) { |_| raise "recovery must not run" }
        end
      end

      expect {
        organizer.call
      }.to raise_error(RuntimeError, "kaboom")

      expect($ledger.events.grep(/^recovery:/)).to be_empty
      expect($ledger.rollback_names).to eq(%w[done main])
      assert_exactly_one_rollback_each
    end
  end

  describe "shallow restoration" do
    it "restores top-level keys but keeps in-place object mutations" do
      main = build_step("main")
      mutating = build_step("mutating",
        on_call: ->(c) {
          c.payload[:items] << "inside-subflow"
          c.added_key = "value"
        },
        failure: { error: "failed" })

      organizer = build_organizer do
        define_method(:call) do
          context.payload = { items: [] }
          main.call!(context)
          context.attempt(mutating)
        end
      end

      result = organizer.call

      expect(result).to be_success
      expect(result.to_h).not_to include(:added_key)
      expect(result.payload[:items]).to eq(["inside-subflow"])
    end
  end
end
