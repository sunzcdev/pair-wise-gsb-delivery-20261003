# frozen_string_literal: true

module CiWatchdog
  TIMEOUT = Float(ENV.fetch('DNSRUBY_TEST_TIMEOUT'))
  raise ArgumentError, 'DNSRUBY_TEST_TIMEOUT must be positive and finite' unless TIMEOUT.positive? && TIMEOUT.finite?

  def run
    label = "#{self.class}##{name}"
    started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    $stderr.puts("START #{label}")
    $stderr.flush
    watchdog = Thread.new do
      sleep TIMEOUT
      $stderr.puts("\nTIMEOUT after #{TIMEOUT}s: #{label}")
      Thread.list.each do |thread|
        $stderr.puts("#{thread.inspect}\n#{Array(thread.backtrace).join("\n")}")
      end
      $stderr.flush
      # Raising in the test thread can get stuck again in teardown or while
      # Open3 waits for a child. End this failed run after preserving its stacks.
      exit! 124
    end
    super
  ensure
    watchdog&.kill
    watchdog&.join
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
    $stderr.puts("DONE #{label} (#{format('%.3f', elapsed)}s)")
    $stderr.flush
  end
end

Minitest::Test.prepend(CiWatchdog)
