$roda_app.opts[:loaded] << :a_c
$roda_app.hash_branch('/a', 'c'){|r| 'a-c'}
