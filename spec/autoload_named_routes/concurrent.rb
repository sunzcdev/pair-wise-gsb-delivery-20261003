$roda_app.opts[:loaded] << :concurrent
sleep 0.2
$roda_app.route(:concurrent){|r| 'concurrent'}
