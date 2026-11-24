# frozen_string_literal: true

RSpec.describe "bundle lock --update-platform-variants" do
  before do
    gemfile <<~G
      source "https://gem.repo4"

      gem "native"
    G

    lockfile <<~L
      GEM
        remote: https://gem.repo4/
        specs:
          native (1.0)
            shared (>= 1.0)
          native (1.0-x64-mingw-ucrt)
            shared (>= 1.0)
          shared (1.5)

      PLATFORMS
        ruby
        x64-mingw-ucrt

      DEPENDENCIES
        native

      BUNDLED WITH
        #{Bundler::VERSION}
    L
  end

  def build_native_variants(extra_windows_deps: [], shared_requirement: nil)
    deps = extra_windows_deps.map {|name, req| [name, req] }
    requirement = shared_requirement

    build_repo4 do
      build_gem "shared", %w[1.5 2.0]

      build_gem "native", "1.0" do |s|
        s.add_dependency "shared", ">= 1.0"
      end

      build_gem "native", "1.0" do |s|
        s.platform = "x64-mingw-ucrt"
        s.add_dependency "shared", ">= 1.0"
      end

      build_gem "native", "1.1" do |s|
        s.platform = "x64-mingw-ucrt"
        deps.each {|name, req| s.add_dependency name, req }
        s.add_dependency "shared", requirement || ">= 1.0"
      end

      deps.each do |name, _req|
        build_gem name, "1.0"
      end
    end
  end

  def installed_specs
    specs_dir = system_gem_path("specifications")
    Dir.children(specs_dir).map {|file| file.sub(/\.gemspec\z/, "") }
  end

  it "re-selects the Windows variant and lets in a gem only Windows uses" do
    build_native_variants(extra_windows_deps: [["local-helper", ">= 1"]])

    bundle "lock --update-platform-variants x64-mingw-ucrt"

    expect(lockfile).to include("native (1.1-x64-mingw-ucrt)")
    expect(lockfile).to include("local-helper (1.0)")
    # The ruby branch and the shared version stay put.
    expect(lockfile).to include("native (1.0)\n")
    expect(lockfile).not_to include("native (1.1)\n")
    expect(lockfile).to include("shared (1.5)\n")
    expect(lockfile).not_to include("shared (2.0)")
  end

  it "keeps the ruby spec and the shared version when re-selecting the Windows variant" do
    build_native_variants(extra_windows_deps: [["local-helper", ">= 1"]])

    bundle "lock --update-platform-variants x64-mingw-ucrt"

    allow(Bundler::SharedHelpers).to receive(:find_gemfile).and_return(bundled_app_gemfile)
    locked = the_bundle.locked_gems.specs.map(&:full_name)
    expect(locked).to include("native-1.0", "native-1.1-x64-mingw-ucrt", "shared-1.5")
    expect(locked.join("\n")).not_to include("shared-2.0")
  end

  it "installs each platform's own variant from the re-selected lockfile" do
    build_native_variants(extra_windows_deps: [["local-helper", ">= 1"]])

    simulate_platform("x64-mingw-ucrt") do
      bundle "lock --update-platform-variants x64-mingw-ucrt"
      bundle "install"

      expect(out).to include("Installing native 1.1 (x64-mingw-ucrt)")
      expect(out).to include("Installing local-helper 1.0")
      expect(installed_specs).to include("native-1.1-x64-mingw-ucrt", "local-helper-1.0", "shared-1.5")
    end

    pristine_system_gems

    simulate_platform("x86_64-linux") do
      bundle "install"

      # The ruby branch is untouched: same gem, same shared version, and none of
      # the Windows-only gem.
      expect(out).to include("Installing native 1.0")
      expect(out).not_to include("local-helper")
      expect(installed_specs).to include("native-1.0", "shared-1.5")
      expect(installed_specs).not_to include("local-helper-1.0")
    end
  end

  it "reads the same re-selected lockfile for both locked platforms" do
    build_native_variants(extra_windows_deps: [["local-helper", ">= 1"]])

    bundle "lock --update-platform-variants x64-mingw-ucrt"

    allow(Bundler::SharedHelpers).to receive(:find_gemfile).and_return(bundled_app_gemfile)

    %w[x64-mingw-ucrt x86_64-linux].each do |platform|
      simulate_platform(platform) do
        definition = Bundler.definition
        specs = definition.resolve.for(definition.current_dependencies, [Gem::Platform.new(platform)])

        native = specs.find {|spec| spec.name == "native" }
        shared = specs.find {|spec| spec.name == "shared" }

        expect(shared.version.to_s).to eq("1.5")

        if platform == "x64-mingw-ucrt"
          expect(native.version.to_s).to eq("1.1")
          expect(native.platform.to_s).to eq("x64-mingw-ucrt")
        else
          expect(native.version.to_s).to eq("1.0")
          expect(native.platform.to_s).to eq("ruby")
        end
      end
    end
  end

  it "reports both sides of the conflict and leaves the lockfile untouched" do
    build_native_variants(shared_requirement: ">= 2.0")

    before = lockfile

    bundle "lock --update-platform-variants x64-mingw-ucrt", raise_on_error: false

    expect(last_command).to be_failure
    expect(err).to include("shared (>= 2.0)")
    expect(err).to include("native-1.1-x64-mingw-ucrt")
    expect(err).to include("native-1.0 requires shared (>= 1.0)")
    expect(err).to include("shared (1.5)")
    expect(lockfile).to eq(before)
  end

  it "writes the exact lockfile expected from the successful re-selection" do
    build_native_variants(extra_windows_deps: [["local-helper", ">= 1"]])

    bundle "lock --update-platform-variants x64-mingw-ucrt"

    expect(lockfile).to eq <<~L
      GEM
        remote: https://gem.repo4/
        specs:
          local-helper (1.0)
          native (1.0)
            shared (>= 1.0)
          native (1.1-x64-mingw-ucrt)
            local-helper (>= 1)
            shared (>= 1.0)
          shared (1.5)

      PLATFORMS
        ruby
        x64-mingw-ucrt

      DEPENDENCIES
        native

      BUNDLED WITH
        #{Bundler::VERSION}
    L
  end

  it "refuses to combine --update-platform-variants with --update" do
    bundle "lock --update-platform-variants x64-mingw-ucrt --update", raise_on_error: false

    expect(last_command).to be_failure
    expect(err).to include("--update-platform-variants can't be used with --update")
  end

  it "refuses to combine --update-platform-variants with --add-platform" do
    bundle "lock --update-platform-variants x64-mingw-ucrt --add-platform java", raise_on_error: false

    expect(last_command).to be_failure
    expect(err).to include("--update-platform-variants can't be used with --add-platform")
  end

  it "refuses an unknown platform" do
    bundle "lock --update-platform-variants unknown", raise_on_error: false

    expect(last_command).to be_failure
    expect(err).to include("is unknown to RubyGems")
  end

  it "refuses a platform that is not locked" do
    bundle "lock --update-platform-variants java", raise_on_error: false

    expect(last_command).to be_failure
    expect(err).to include("since it's not locked")
  end

  it "prints the re-selected lockfile without writing it with --print" do
    build_native_variants(extra_windows_deps: [["local-helper", ">= 1"]])

    before = lockfile

    bundle "lock --update-platform-variants x64-mingw-ucrt --print"

    expect(out).to include("native (1.1-x64-mingw-ucrt)")
    expect(out).to include("local-helper (1.0)")
    expect(lockfile).to eq(before)
  end
end
