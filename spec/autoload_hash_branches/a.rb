$roda_app.opts[:loaded] << :a
$roda_app.hash_branch('a'){|r| r.hash_branches; 'a'}
