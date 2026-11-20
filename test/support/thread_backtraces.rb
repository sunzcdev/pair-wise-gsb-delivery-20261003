# frozen_string_literal: true

# SIGUSR2 is not available on all platforms (i.e. Windows); in such cases, the
# debugging hook is simply skipped instead of breaking the whole test suite.
if Signal.list.key?("USR2")
  Signal.trap("USR2") do
    warn "starting..."
    Thread.list.each do |thread|
      warn "Thread TID-#{(thread.object_id ^ Process.pid).to_s(36)} #{thread.name}"
      warn thread.backtrace || "<no backtrace available>"
    end
  end
end
