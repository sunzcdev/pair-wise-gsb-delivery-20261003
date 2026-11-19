$roda_app.opts[:loaded] << :a_d
$roda_app.route(:d, :a){|r| 'a-d'}
