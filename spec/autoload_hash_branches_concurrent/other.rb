$roda_concurrent_loaded_count += 1
# Prove this branch finishes while the +slow+ branch is still inside its load.
$roda_concurrent_other_finished << true
$roda_app.hash_branch('other') { |r| 'other-loaded' }
