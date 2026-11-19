$roda_app.opts[:loaded] << :b
$roda_app.route(:b){|r| 'b'}
