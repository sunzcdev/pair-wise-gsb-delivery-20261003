# frozen_string_literal: true

require 'timeout'

module ForkTestHelper
  # A timeout started before fork cannot protect the child: its timer thread
  # exists only in the parent. Bound both processes and always reap the child,
  # including when an outer test timeout interrupts the parent's wait.
  def self.run(timeout: 5)
    pid = nil
    Thread.handle_interrupt(Exception => :never) do
      pid = fork do
        Thread.handle_interrupt(Exception => :immediate) do
          begin
            Timeout.timeout(timeout) { yield }
            exit! 0
          rescue Exception => error
            warn error.full_message
            exit! 1
          end
        end
      end
    end
    Timeout.timeout(timeout + 2) { Process.wait2(pid).last }
  ensure
    Thread.handle_interrupt(Exception => :never) do
      begin
        if pid && !Process.waitpid(pid, Process::WNOHANG)
          Process.kill('KILL', pid)
          Process.waitpid(pid)
        end
      rescue Errno::ECHILD, Errno::ESRCH
        # A successful wait already reaped this child.
      end
    end
  end
end
