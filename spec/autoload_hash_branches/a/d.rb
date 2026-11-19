$roda_app.opts[:loaded] << :a_d
$roda_app.hash_branch('/a', 'd'){|r| 'a-d'}
