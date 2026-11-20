$roda_app.opts[:loaded] << :fail
$roda_app.route(:fail_half){|r| 'fail-half'}
raise "autoload named route load failure"
$roda_app.route(:fail_missing){|r| 'fail-missing'}
