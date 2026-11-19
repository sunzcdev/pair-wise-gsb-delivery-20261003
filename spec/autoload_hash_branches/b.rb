$roda_app.opts[:loaded] << :b
$roda_app.hash_branch('b'){|r| 'b'}
