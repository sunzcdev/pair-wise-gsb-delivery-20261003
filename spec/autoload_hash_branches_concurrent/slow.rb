$roda_concurrent_loaded_count += 1
# Wait until the test releases the branch's load, after concurrent requests
# have had a chance to block on it.
$roda_concurrent_entered.arrive
$roda_concurrent_loaded.wait_for
$roda_app.hash_branch('slow') { |r| 'slow-loaded' }
