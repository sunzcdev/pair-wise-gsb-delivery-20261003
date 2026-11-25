# frozen_string_literal: true

require "dry/system/provider_sources/settings/loader"

RSpec.describe Dry::System::ProviderSources::Settings::Loader do
  subject(:loader) { described_class.new(root: root, env: env, store: store) }

  let(:root) { make_tmp_directory }
  let(:env) { :development }
  let(:store) { {} }

  describe "#initialize" do
    context "non-test environment" do
      let(:env) { :development }

      it "exposes the environment as the highest precedence source, followed by .env files" do
        write(root.join(".env"), "LOW=from-env-file\n")
        write(root.join(".env.development"), "MID=from-dev\n")

        expect(loader.sources.map(&:name)).to eq [:env, ".env.development", ".env"]
      end

      it "does not mutate the live environment when reading .env files" do
        write(root.join(".env"), "SOME_KEY=from-file\n")

        expect { loader }.not_to(change { ENV.key?("SOME_KEY") })
      end
    end

    context "test environment" do
      let(:env) { :test }

      it "does not read .env.local" do
        write(root.join(".env.local"), "SKIPPED=nope\n")
        write(root.join(".env.test"), "KEPT=yes\n")

        expect(loader.sources.map(&:name)).to include(".env.test")
        expect(loader.sources.map(&:name)).not_to include(".env.local")
      end
    end

    context "multiple roots" do
      subject(:loader) { described_class.new(root: root, env: env, roots: [root, other_root], store: store) }

      let(:other_root) { make_tmp_directory }

      it "keeps each root's files as separate sources in root order" do
        write(root.join(".env"), "SHARED=first\n")
        write(other_root.join(".env"), "SHARED=second\nOTHER=here\n")

        names = loader.sources.map(&:name)

        expect(names).to eq [:env, "root0:.env", "root1:.env"]
      end

      it "keeps each source's values separate" do
        write(root.join(".env"), "SHARED=first\n")
        write(other_root.join(".env"), "SHARED=second\nOTHER=here\n")

        expect(loader.candidates("SHARED").map { |c| [c.source_name, c.value] }).to eq(
          [["root0:.env", "first"], ["root1:.env", "second"]]
        )
      end
    end
  end

  describe "#[]" do
    it "returns the value from the highest precedence source that carries the key" do
      store["SOME_KEY"] = "from-env"
      write(root.join(".env"), "SOME_KEY=from-file\n")

      expect(loader["SOME_KEY"]).to eq "from-env"
    end

    it "falls back to a lower source when the environment does not carry the key" do
      write(root.join(".env"), "SOME_KEY=from-file\n")

      expect(loader["SOME_KEY"]).to eq "from-file"
    end

    it "returns nil when no source carries the key" do
      expect(loader["MISSING_KEY"]).to be_nil
    end
  end

  describe "#candidates" do
    it "returns every source carrying the key with its provenance, highest precedence first" do
      store["SOME_KEY"] = "from-env"
      write(root.join(".env.development"), "SOME_KEY=from-dev\n")
      write(root.join(".env"), "SOME_KEY=from-base\n")

      candidates = loader.candidates("SOME_KEY")

      expect(candidates.map(&:source_name)).to eq [:env, ".env.development", ".env"]
      expect(candidates.map(&:value)).to eq ["from-env", "from-dev", "from-base"]
    end

    it "includes explicitly empty values so they mask lower sources" do
      store["SOME_KEY"] = ""
      write(root.join(".env"), "SOME_KEY=from-file\n")

      candidates = loader.candidates("SOME_KEY")

      expect(candidates.first.source_name).to eq :env
      expect(candidates.first.value).to eq ""
      expect(candidates.first).to be_blank
    end
  end

  describe "environment isolation" do
    it "snapshots the environment so later mutations do not leak into the loader" do
      store["SOME_KEY"] = "original"
      loader

      store["SOME_KEY"] = "mutated"

      expect(loader["SOME_KEY"]).to eq "original"
    end
  end
end