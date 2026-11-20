#!ruby
# A CGI that stays alive long enough for the parent request to be cancelled.
# After cancellation WEBrick must terminate the process and reclaim every
# pump thread and pipe.  The PID is recorded so a test can prove the process
# was reaped instead of being orphaned.
STDIN.binmode
STDOUT.binmode
STDERR.binmode

# The runner clears ENV (so Dir.tmpdir would differ from the parent's); use
# a path next to this script, which both processes can locate deterministically.
pid_file = File.expand_path("webrick_slow_cgi.pid", __dir__)
File.binwrite(pid_file, Process.pid.to_s)

# Ignore friendly termination so a plain :TERM cannot end the script early;
# the handler must still be able to forcibly clean it up.
trap("TERM") { } rescue nil

loop do
  sleep 0.1
end
