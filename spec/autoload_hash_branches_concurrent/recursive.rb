$roda_concurrent_loaded_count += 1
# While this file is being loaded, request the same branch again.  The branch
# is still loading, so the recursive autoload cannot succeed.
$roda_app.call('REQUEST_METHOD' => 'GET', 'PATH_INFO' => '/recursive', 'SCRIPT_NAME' => '', 'rack.input' => StringIO.new)
$roda_app.hash_branch('recursive') { |r| 'recursive-loaded' }
