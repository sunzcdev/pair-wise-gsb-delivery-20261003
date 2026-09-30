$roda_concurrent_loaded_count += 1
# Wait until the test confirms the waiting requests are blocked on the load,
# before registering anything.
$roda_concurrent_started.arrive
$roda_concurrent_continue.wait_for

# Register a branch before failing.  If the rollback contract is broken, this
# half-registered route becomes reachable.
$roda_app.hash_branch('failing') { |r| 'failing-half-registered' }
unless $roda_concurrent_allow_failure_success
  raise "boom"
end
$roda_app.hash_branch('failing') { |r| 'failing-recovered' }
