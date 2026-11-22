require_relative "../spec_helper"
require 'fileutils'

run_tests = true
begin
  begin
    require 'tilt'
  rescue LoadError
    warn "tilt not installed, skipping assets plugin test"
    run_tests = false
  end
end

if run_tests
  pid_dir = "spec/pid-#{$$}"
  assets_dir = File.join(pid_dir, "tmp")
  metadata_file = File.expand_path(File.join(assets_dir, 'precompiled.json'))
  describe 'recheck_precompiled_assets plugin' do
    define_method(:compile_assets) do |opts={}|
      Class.new(Roda) do
        plugin :assets, **{:css => 'app.str', :path => assets_dir, :css_dir=>nil, :precompiled=>metadata_file, :public=>assets_dir, :prefix=>nil, :js_compressor=>:none, :css_compressor=>:none}.merge!(opts)
        compile_assets
      end
    end

    before do
      Dir.mkdir(pid_dir) unless File.directory?(pid_dir)
      Dir.mkdir(assets_dir) unless File.directory?(assets_dir)
      FileUtils.cp('spec/assets/css/app.str', assets_dir)
      FileUtils.cp('spec/assets/js/head/app.js', assets_dir)
      compile_assets
      File.utime(Time.now, Time.now - 20, metadata_file)

      app(:bare) do
        plugin :assets, :public => assets_dir, :prefix=>nil, :precompiled=>metadata_file
        plugin :recheck_precompiled_assets

        route do |r|
          r.assets
          "#{assets(:css)}\n#{assets(:js)}"
        end
      end
    end
    after do
      FileUtils.rm_r(pid_dir) if File.directory?(pid_dir)
    end

    [true, false].each do |frozen|
      it "should support :recheck_precompiled option to recheck precompiled file for new precompilation data when app is #{frozen ? "" : "un"}frozen" do
        if frozen
          app.plugin :freeze_request_response
          app.freeze
        end
        css_hash = app.assets_config.compiled['css']
        app.assets_config.compiled['js'].must_be_nil
        body.scan("href=\"/app.#{css_hash}.css\"").length.must_equal 1
        body("/app.#{css_hash}.css").must_match(/color:\s*red/)

        css_file = File.join(assets_dir, 'app.str')
        File.write(css_file, File.read(css_file).sub('red', 'blue'))
        compile_assets
        File.utime(Time.now, Time.now - 10, metadata_file)

        app.assets_config.compiled['css'].must_equal css_hash
        body.scan("href=\"/app.#{css_hash}.css\"").length.must_equal 1
        body("/app.#{css_hash}.css").must_match(/color:\s*red/)
        css2_hash = nil

        2.times do
          app.recheck_precompiled_assets
          css2_hash = app.assets_config.compiled['css']
          css2_hash.wont_equal css_hash
          body.scan("href=\"/app.#{css2_hash}.css\"").length.must_equal 1
          body("/app.#{css2_hash}.css").must_match(/color:\s*blue/)
          body("/app.#{css_hash}.css").must_match(/color:\s*red/)
        end

        compile_assets(:js=>'app.js', :js_dir=>nil)
        app.recheck_precompiled_assets
        js_hash = app.assets_config.compiled['js']
        body.scan("src=\"/app.#{js_hash}.js\"").length.must_equal 1
        body("/app.#{js_hash}.js").must_match(/console\.log\(.test.\)/)

        app.assets_config.compiled.replace({})
        app.compile_assets
        body.strip.must_be_empty
        next if frozen

        app.plugin :assets, :css => 'app.str', :path => assets_dir, :css_dir=>nil, :css_opts => {:cache=>false}
        app.compile_assets
        body.scan("href=\"/app.#{css2_hash}.css\"").length.must_equal 1
        body("/app.#{css2_hash}.css").must_match(/color:\s*blue/)
      end
    end
  end

  describe 'recheck_precompiled_assets plugin' do
    it "should not allow loading if not using assets plugin" do
      proc{app(:recheck_precompiled_assets)}.must_raise Roda::RodaError
    end

    it "should not allow loading if using assets plugin without :precompiled option" do
      proc do
        app(:bare) do
          plugin :assets
          plugin :recheck_precompiled_assets
        end
      end.must_raise Roda::RodaError
    end
  end

  describe 'recheck_precompiled_assets plugin manifest validation and atomic switching' do
    pid_dir = "spec/pid-#{$$}-manifest"
    assets_dir = File.join(pid_dir, "tmp")
    metadata_file = File.expand_path(File.join(assets_dir, 'precompiled.json'))

    define_method(:compile_assets) do |opts={}|
      Class.new(Roda) do
        plugin :assets, **{:css => 'app.str', :path => assets_dir, :css_dir=>nil, :precompiled=>metadata_file, :public=>assets_dir, :prefix=>nil, :js_compressor=>:none, :css_compressor=>:none}.merge!(opts)
        compile_assets
      end
    end

    define_method(:build_app) do |plugin_opts={}|
      app(:bare) do
        plugin :assets, :public => assets_dir, :prefix=>nil, :precompiled=>metadata_file
        plugin :assets_preloading
        plugin :recheck_precompiled_assets, plugin_opts

        route do |r|
          r.assets
          r.is 'preload' do
            self.class.opts[:_block_request].call if self.class.opts[:_block_request]
            response['Link'] = preload_assets_link_header(:css)
            preload_assets_link_tags(:css)
          end
          r.is 'tags' do
            "#{assets(:css)}\n#{assets(:js)}"
          end
          response.status = 404
          nil
        end
      end
    end

    define_method(:touch_metadata) do
      # Ensure the metadata mtime changes so recheck_precompiled_assets notices it.
      File.utime(Time.now + 30, Time.now + 30, metadata_file)
    end

    define_method(:css_digest_in) do |html|
      html[%r{/app\.([0-9a-fA-F]+)\.css}, 1]
    end

    before do
      Dir.mkdir(pid_dir) unless File.directory?(pid_dir)
      Dir.mkdir(assets_dir) unless File.directory?(assets_dir)
      FileUtils.cp('spec/assets/css/app.str', assets_dir)
      FileUtils.cp('spec/assets/js/head/app.js', assets_dir)
      compile_assets(:js=>'app.js', :js_dir=>nil)
      File.utime(Time.now, Time.now - 20, metadata_file)
      build_app
    end
    after do
      FileUtils.rm_r(pid_dir) if File.directory?(pid_dir)
    end

    it "switches preload hints and static routes to the same new version atomically" do
      v1_digest = app.assets_config.compiled['css']
      v1_digest.wont_be_nil

      link = header('Link', '/preload')
      link.must_include "</app.#{v1_digest}.css>;rel=preload;as=style"
      body("/app.#{v1_digest}.css").must_match(/color:\s*red/)

      # Build a second, distinct version of the asset (red -> blue).  The old, content-addressed
      # file represents a previously deployed build artifact that must remain readable, so it is
      # preserved on disk alongside the new file.
      css_file = File.join(assets_dir, 'app.str')
      v1_compiled_file = File.join(assets_dir, "app.#{v1_digest}.css")
      v1_compiled_content = File.read(v1_compiled_file)
      File.write(css_file, "body { color: blue; }\n")
      compile_assets(:js=>'app.js', :js_dir=>nil)
      touch_metadata
      v2_digest = JSON.parse(File.read(metadata_file))['css']
      v2_digest.wont_equal v1_digest
      # Restore the previous build artifact, as a deploy would keep prior files on disk.
      File.write(v1_compiled_file, v1_compiled_content)
      File.read(v1_compiled_file).must_match(/color:\s*red/)
      File.read(File.join(assets_dir, "app.#{v2_digest}.css")).must_match(/color:\s*blue/)

      # Before recheck, the app still hints and serves the old version.
      css_digest_in(body('/preload')).must_equal v1_digest
      body("/app.#{v1_digest}.css").must_match(/color:\s*red/)

      swapped = app.recheck_precompiled_assets
      swapped.must_equal true

      # After the switch, the preload hint and the readable file are the same version.
      link = header('Link', '/preload')
      link.must_include "</app.#{v2_digest}.css>;rel=preload;as=style"
      link.wont_include v1_digest
      css_digest_in(body('/preload')).must_equal v2_digest
      body("/app.#{v2_digest}.css").must_match(/color:\s*blue/)

      # The previously generated URL is still requestable within the retained set,
      # and still serves its original (v1) content from the old file on disk.
      body("/app.#{v1_digest}.css").must_match(/color:\s*red/)
    end

    it "keeps the previous manifest when the new manifest drops a retained asset" do
      v1_digest = app.assets_config.compiled['css']
      js_digest = app.assets_config.compiled['js']

      # Write a manifest that drops the css entry (new manifest has fewer items).
      touch_metadata
      File.write(metadata_file, JSON.generate('js' => js_digest))

      swapped = app.recheck_precompiled_assets
      swapped.must_equal false

      # The old manifest is still in use for both hints and routes.
      app.assets_config.compiled['css'].must_equal v1_digest
      css_digest_in(body('/preload')).must_equal v1_digest
      body("/app.#{v1_digest}.css").must_match(/color:\s*red/)
      status("/app.#{v1_digest}.css").must_equal 200
    end

    it "only enforces explicitly retained assets when :retained_assets is given" do
      build_app(:retained_assets => [:js])
      css_digest = app.assets_config.compiled['css']
      js_digest = app.assets_config.compiled['js']

      # New manifest keeps js but drops css; js is the only retained key, so it applies.
      touch_metadata
      File.write(metadata_file, JSON.generate('js' => js_digest))

      app.recheck_precompiled_assets.must_equal true
      app.assets_config.compiled['js'].must_equal js_digest
      app.assets_config.compiled['css'].must_be_nil

      # The retained js URL still serves its content; the dropped css is gone.
      body("/app.#{js_digest}.js").must_match(/console\.log\(.test.\)/)
      status("/app.#{css_digest}.css").must_equal 404
    end

    it "picks up a corrected manifest after an earlier one was rejected" do
      v1_digest = app.assets_config.compiled['css']
      js_digest = app.assets_config.compiled['js']

      # First new manifest is invalid because it drops the retained css entry.
      touch_metadata
      File.write(metadata_file, JSON.generate('js' => js_digest))
      app.recheck_precompiled_assets.must_equal false
      app.assets_config.compiled['css'].must_equal v1_digest

      # A corrected manifest that restores css is still applied on the next recheck.
      corrected = JSON.generate('css' => v1_digest, 'js' => js_digest)
      touch_metadata
      File.write(metadata_file, corrected)
      app.recheck_precompiled_assets.must_equal true
      app.assets_config.compiled['css'].must_equal v1_digest
      app.assets_config.compiled['js'].must_equal js_digest
      css_digest_in(body('/preload')).must_equal v1_digest
    end

    it "presents a consistent version to a request blocked across the switch point" do
      v1_digest = app.assets_config.compiled['css']

      request_started = Queue.new
      release_request = Queue.new
      observed = {}

      request_thread = Thread.new do
        # Block inside the route while the manifest switch happens in the main thread.
        app.opts[:_block_request] = lambda do
          request_started << true
          release_request.pop
        end

        response = app.call(
          "REQUEST_METHOD" => "GET",
          "PATH_INFO" => "/preload",
          "SCRIPT_NAME" => "",
          "rack.input" => rack_input,
          "rack.errors" => StringIO.new,
          "rack.url_scheme" => "http"
        )
        observed[:status] = response[0]
        observed[:link] = response[1]['Link']
        body = String.new
        response[2].each { |chunk| body << chunk }
        response[2].close if response[2].respond_to?(:close)
        observed[:body] = body
      end

      # Wait until the in-flight request has entered the blocked route.
      request_started.pop

      # Build and apply the second version while the request is blocked.
      css_file = File.join(assets_dir, 'app.str')
      File.write(css_file, File.read(css_file).sub('red', 'blue'))
      compile_assets(:js=>'app.js', :js_dir=>nil)
      touch_metadata
      v2_digest = JSON.parse(File.read(metadata_file))['css']
      v2_digest.wont_equal v1_digest
      app.recheck_precompiled_assets.must_equal true

      # Release the blocked request and collect what it observed.
      release_request << true
      request_thread.join

      observed[:status].must_equal 200
      observed[:link].must_include "</app.#{v2_digest}.css>;rel=preload;as=style"
      hinted_digest = css_digest_in(observed[:body])
      hinted_digest.must_equal v2_digest

      # The exact URL advertised by the Link header must be readable and serve v2 content.
      body("/app.#{hinted_digest}.css").must_match(/color:\s*blue/)
      body("/app.#{v2_digest}.css").must_match(/color:\s*blue/)
    end
  end
end
