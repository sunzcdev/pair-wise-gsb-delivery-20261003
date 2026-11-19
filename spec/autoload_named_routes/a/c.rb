$roda_app.opts[:loaded] << :a_c
$roda_app.route(:c, :a){|r| 'a-c'}
