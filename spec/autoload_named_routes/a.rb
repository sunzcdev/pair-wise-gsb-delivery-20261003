$roda_app.opts[:loaded] << :a
$roda_app.route(:a) do |r|
  r.on('c'){r.route(:c, :a)}
  r.on('d'){r.route(:d, :a)}
  r.on('e'){r.route(:e, :a)}
  'a'
end
