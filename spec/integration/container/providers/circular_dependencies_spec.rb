# frozen_string_literal: true

require "thread"

RSpec.describe "Providers / Circular dependencies and concurrent resolution" do
  # Thread coordination without sleeps inside the provider: a provider signals when its
  # step has begun, then blocks until the test releases it, giving deterministic control
  # over when waiting threads observe a running or finishing step.
  def coordination_barrier
    {started: Queue.new, release: Queue.new}
  end

  def wait_for_signal(queue)
    deadline = Time.now + 5
    loop do
      return queue.pop(true)
    rescue ThreadError
      raise "timed out waiting for provider step to start" if Time.now > deadline
      sleep 0.01
    end
  end

  def wait_until_blocked(thread)
    deadline = Time.now + 5
    sleep 0.01 until thread.status == "sleep" || Time.now > deadline
    raise "thread never blocked on the provider step" unless thread.status == "sleep"
  end

  # Releases the barrier as soon as the provider step starts. Used by single-threaded
  # examples, where no other thread is available to do the release.
  def release_when_started(barrier)
    Thread.new do
      barrier[:started].pop
      barrier[:release] << true
    end
  end

  def capture_error
    yield
    nil
  rescue Exception => error # rubocop:disable Lint/SuppressedException
    error
  end

  def quiet_thread
    Thread.new do
      Thread.current.report_on_exception = false
      yield
    end
  end

  describe "a genuine circular dependency" do
    subject(:container) {
      cycle = back_edge
      gate = barrier

      Class.new(Dry::System::Container) do
        register_provider :a do
          start do
            target_container[:b]
            register(:a, "a")
          end
        end

        register_provider :b do
          start do
            gate[:started] << true
            gate[:release].pop
            target_container[:a] if cycle
            register(:b, "b")
          end
        end
      end
    }

    let(:back_edge) { true }
    let(:barrier) { coordination_barrier }

    it "raises an error naming every provider key involved in the cycle" do
      release_when_started(barrier)

      expect { container[:a] }.to raise_error(
        Dry::System::ProviderCircularDependencyError,
        /:a -> :b -> :a/
      )
    end

    it "wakes a second thread waiting on the same provider with the same failure" do
      first = quiet_thread { container[:a] }
      wait_for_signal(barrier[:started])

      second = quiet_thread { container[:a] }
      wait_until_blocked(second)

      barrier[:release] << true

      first_error = capture_error { first.join }
      second_error = capture_error { second.join }

      expect(first_error).to be_a(Dry::System::ProviderCircularDependencyError)
      expect(second_error).to be_a(Dry::System::ProviderCircularDependencyError)
      expect(second_error.message).to eq(first_error.message)
      expect(first_error.message).to include(":a -> :b -> :a")
    end

    it "leaves neither provider started so the failed round can be corrected and retried" do
      first = quiet_thread { container[:a] }
      wait_for_signal(barrier[:started])

      second = quiet_thread { container[:a] }
      wait_until_blocked(second)

      barrier[:release] << true

      expect(capture_error { first.join }).to be_a(Dry::System::ProviderCircularDependencyError)
      expect(capture_error { second.join }).to be_a(Dry::System::ProviderCircularDependencyError)

      expect(container.providers[:a]).not_to be_started
      expect(container.providers[:b]).not_to be_started
    end

    context "after the back edge is removed" do
      it "resolves normally with corrected dependencies" do
        release_when_started(barrier)
        expect { container[:a] }.to raise_error(Dry::System::ProviderCircularDependencyError)

        # A fresh container encodes the corrected dependency (provider B no longer
        # reaches back for provider A).
        corrected = Class.new(Dry::System::Container) do
          register_provider :a do
            start do
              target_container[:b]
              register(:a, "a")
            end
          end

          register_provider :b do
            start do
              register(:b, "b")
            end
          end
        end

        expect(corrected[:b]).to eq("b")
        expect(corrected[:a]).to eq("a")
        expect(corrected.providers[:a]).to be_started
        expect(corrected.providers[:b]).to be_started
      end

      it "retries the failed provider in the same container once the edge is gone" do
        release_when_started(barrier)
        expect { container[:a] }.to raise_error(Dry::System::ProviderCircularDependencyError)

        # Replace provider B with a corrected implementation lacking the back edge.
        corrected_source = Dry::System::Provider::Source.for(name: :b) do
          start do
            register(:b, "b")
          end
        end

        corrected_provider = Dry::System::Provider.new(
          name: :b,
          target_container: container,
          source_class: corrected_source
        )
        container.providers.providers[:b] = corrected_provider

        expect(container[:a]).to eq("a")
        expect(container[:b]).to eq("b")
        expect(container.providers[:a]).to be_started
      end

      it "does not re-run independent providers that already succeeded" do
        runs = Hash.new(0)

        independent = Class.new(Dry::System::Container) do
          register_provider(:independent) do
            start do
              runs[:independent] += 1
              register(:independent, "independent")
            end
          end
        end
        independent[:independent]

        first = quiet_thread { container[:a] }
        wait_for_signal(barrier[:started])

        second = quiet_thread { container[:a] }
        wait_until_blocked(second)

        barrier[:release] << true

        expect(capture_error { first.join }).to be_a(Dry::System::ProviderCircularDependencyError)
        expect(capture_error { second.join }).to be_a(Dry::System::ProviderCircularDependencyError)

        corrected = Class.new(Dry::System::Container) do
          register_provider :a do
            start do
              target_container[:b]
              register(:a, "a")
            end
          end

          register_provider :b do
            start do
              register(:b, "b")
            end
          end
        end
        corrected[:a]

        expect(runs[:independent]).to eq(1)
      end
    end
  end

  describe "two threads sharing one provider's initialization" do
    it "does not mistake waiting for a shared initialization for a cycle" do
      gate = coordination_barrier

      container = Class.new(Dry::System::Container) do
        register_provider :a do
          start do
            target_container[:b]
            register(:a, "a")
          end
        end

        register_provider :b do
          start do
            target_container[:c]
            register(:b, "b")
          end
        end

        register_provider :c do
          start do
            gate[:started] << true
            gate[:release].pop
            register(:c, "c")
          end
        end
      end

      first = Thread.new { container[:c] }
      wait_for_signal(gate[:started])

      second = Thread.new { container[:a] }
      wait_until_blocked(second)

      gate[:release] << true

      expect(first.value).to eq("c")
      expect(second.value).to eq("a")
    end

    it "shares a failed initialization with every thread waiting on the provider" do
      gate = coordination_barrier

      container = Class.new(Dry::System::Container) do
        register_provider :broken do
          start do
            gate[:started] << true
            gate[:release].pop
            raise "boom"
          end
        end
      end

      first = quiet_thread { container[:broken] }
      wait_for_signal(gate[:started])

      second = quiet_thread { container[:broken] }
      wait_until_blocked(second)

      gate[:release] << true

      first_error = capture_error { first.join }
      second_error = capture_error { second.join }

      expect(first_error).to be_a(RuntimeError).and have_attributes(message: "boom")
      expect(second_error).to be_a(RuntimeError).and have_attributes(message: "boom")

      # A failed attempt does not leave the provider permanently stuck: a later start
      # on a fresh container succeeds.
      retryable = Class.new(Dry::System::Container) do
        register_provider :broken do
          start do
            register(:broken, "recovered")
          end
        end
      end

      expect(retryable[:broken]).to eq("recovered")
    end
  end

  describe "resolving a component with the same root key as its running provider" do
    it "short-circuits the re-entrant step instead of reporting a cycle" do
      dir = make_tmp_directory

      with_directory(dir) do
        write "lib/animals/cat.rb", <<~RUBY
          module Test
            module Animals
              class Cat
                # Resolving this component starts the "animals" provider, whose own step
                # is already running on this thread.
                def initialize(collar = Test::Container["animals.collar"])
                  @collar = collar
                end

                attr_reader :collar
              end
            end
          end
        RUBY

        write "lib/animals/collar.rb", <<~RUBY
          module Test
            module Animals
              class Collar; end
            end
          end
        RUBY
      end

      root = dir
      Test::Container = Class.new(Dry::System::Container) do
        configure do |config|
          config.root = root
          config.component_dirs.add "lib" do |component_dir|
            component_dir.namespaces.add_root const: "test"
          end
        end

        register_provider :animals, namespace: true do
          start do
            require "animals/cat"
            register :cat, Test::Animals::Cat.new
            register :collar, Test::Animals::Collar.new
          end
        end
      end

      expect { Test::Container["animals.cat"] }.not_to raise_error
      expect(Test::Container["animals.cat"]).to be_a(Test::Animals::Cat)
      expect(Test::Container["animals.cat"].collar).to be_a(Test::Animals::Collar)
    end
  end
end
