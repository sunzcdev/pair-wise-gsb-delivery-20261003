# encoding: binary

module AMQ
  module Protocol
    RSpec.describe Registry do
      # Two versions sharing class and method IDs, declared the other way
      # around, are the sharpest test that lookups and codecs follow a
      # description and not a global constant.
      def thing_document(version:, port:, first:)
        {
          "name" => "ThingAMQP",
          "major-version" => version[0],
          "minor-version" => version[1],
          "revision" => version[2],
          "port" => port,
          "constants" => [{ "name" => "FRAME-END", "value" => 206 }],
          "classes" => [
            {
              "name" => "thing",
              "id" => 77,
              "methods" => [
                {
                  "name" => "make",
                  "id" => 10,
                  "arguments" => first
                }
              ],
              "properties" => [
                { "type" => "shortstr", "name" => "content-type" }
              ]
            }
          ]
        }
      end

      let(:v1_document) do
        thing_document(
          version: [1, 0, 0], port: 5001,
          first: [
            { "type" => "shortstr", "name" => "alpha" },
            { "type" => "long", "name" => "beta" }
          ]
        )
      end

      let(:v2_document) do
        thing_document(
          version: [2, 0, 0], port: 5002,
          first: [
            { "type" => "long", "name" => "beta" },
            { "type" => "shortstr", "name" => "alpha" }
          ]
        )
      end

      before { described_class.clear! }

      describe ".load" do
        it "loads each version into its own namespace" do
          v1 = described_class.load("1.0.0", documents: [v1_document])
          v2 = described_class.load("2.0.0", documents: [v2_document])

          expect(v1.namespace).not_to equal(v2.namespace)
          expect(v1.method_class(77, 10)).not_to equal(v2.method_class(77, 10))
          expect(v1.port).to eq(5001)
          expect(v2.port).to eq(5002)
        end

        it "keeps class and method IDs bound to the version that declares them" do
          v1 = described_class.load("1.0.0", documents: [v1_document])
          v2 = described_class.load("2.0.0", documents: [v2_document])

          expect(v1.method(77, 10).argument_names).to eq(%i[alpha beta])
          expect(v2.method(77, 10).argument_names).to eq(%i[beta alpha])
          expect(v1.method(77, 10)).not_to eq(v2.method(77, 10))
        end

        it "encodes and decodes with the argument order of the selected version" do
          v1 = described_class.load("1.0.0", documents: [v1_document])
          v2 = described_class.load("2.0.0", documents: [v2_document])

          v1_wire = v1.method_class(77, 10).encode_arguments("s", 5)
          v2_wire = v2.method_class(77, 10).encode_arguments(5, "s")

          expect(v1_wire).not_to eq(v2_wire)
          expect(v1.method_class(77, 10).decode(v1_wire).to_h).to eq(alpha: "s", beta: 5)
          expect(v2.method_class(77, 10).decode(v2_wire).to_h).to eq(beta: 5, alpha: "s")
        end

        it "reuses the namespace when the same description is loaded twice" do
          v1 = described_class.load("1.0.0", documents: [v1_document])
          v1_again = described_class.load("1.0.0", documents: [v1_document])

          expect(v1_again).to equal(v1)
          expect(v1_again.namespace).to equal(v1.namespace)
        end

        it "rejects a different description under an already loaded label" do
          described_class.load("1.0.0", documents: [v1_document])

          expect {
            described_class.load("1.0.0", documents: [v1_document.merge("port" => 9999)])
          }.to raise_error(Registry::ConflictError, /already loaded/)
        end

        it "rejects a document whose version does not match the requested one" do
          expect {
            described_class.load("3.0.0", documents: [v1_document])
          }.to raise_error(ArgumentError, /version 1\.0\.0/)
        end
      end

      describe ".fingerprint" do
        it "is stable for equal descriptions and different across versions" do
          a = Spec::Loader.from_document(v1_document)
          b = Spec::Loader.from_document(v1_document)

          expect(described_class.fingerprint(a)).to eq(described_class.fingerprint(b))
          expect(described_class.fingerprint(a)).not_to eq(described_class.fingerprint(Spec::Loader.from_document(v2_document)))
        end
      end

      describe "#method_at_index" do
        it "resolves the packed 32 bit class/method index of that version" do
          v1 = described_class.load("1.0.0", documents: [v1_document])

          expect(v1.method_at_index((77 << 16) | 10).qualified_name).to eq("thing.make")
          expect(v1.method_at_index(0xFFFF)).to be_nil
        end
      end
    end

    RSpec.describe Spec::Description do
      let(:document) do
        {
          "name" => "ThingAMQP",
          "major-version" => 1,
          "minor-version" => 2,
          "revision" => 3,
          "port" => 4000,
          "domains" => [["label", "shortstr"]],
          "classes" => [
            {
              "name" => "thing",
              "id" => 77,
              "methods" => [
                {
                  "name" => "make",
                  "id" => 10,
                  "arguments" => [
                    { "domain" => "label", "name" => "first" },
                    { "type" => "long", "name" => "second" }
                  ]
                }
              ]
            }
          ]
        }
      end

      subject(:description) { Spec::Loader.from_document(document) }

      it "derives version, preamble and port from the document" do
        expect(description.version).to eq("1.2.3")
        expect(description.port).to eq(4000)
        expect(description.preamble).to eq("AMQP\x00\x01\x02\x03".b)
      end

      it "resolves declared domains against its own table" do
        expect(description.resolve_domain("label")).to eq("shortstr")
        expect(description.method_with_id(77, 10).arguments.first.type).to eq("shortstr")
      end

      it "rejects unknown domains at load time" do
        broken = Marshal.load(Marshal.dump(document))
        broken["classes"][0]["methods"][0]["arguments"][0]["domain"] = "nope"

        expect { Spec::Loader.from_document(broken) }.to raise_error(ArgumentError, /unknown protocol domain/)
      end

      it "keeps field declaration order exactly as written" do
        reordered = Marshal.load(Marshal.dump(document))
        reordered["classes"][0]["methods"][0]["arguments"].reverse!

        expect(Spec::Loader.from_document(reordered).method_with_id(77, 10).argument_names)
          .to eq(%i[second first])
        expect(description.method_with_id(77, 10).argument_names).to eq(%i[first second])
      end

      it "does not treat a declared null default as an absent one" do
        with_null = Marshal.load(Marshal.dump(document))
        with_null["classes"][0]["methods"][0]["arguments"][0]["default-value"] = nil

        field = Spec::Loader.from_document(with_null).method_with_id(77, 10).arguments.first
        expect(field.default?).to eq(true)
        expect(field.default).to be_nil
        expect(description.method_with_id(77, 10).arguments.first.default?).to eq(false)
      end

      it "is equal to an independently loaded copy of the same document" do
        copy = Spec::Loader.from_document(Marshal.load(Marshal.dump(document)))

        expect(copy).to eq(description)
        expect(copy.digest).to eq(description.digest)
        expect(copy.hash).to eq(description.hash)
      end
    end

    RSpec.describe Spec::Loader do
      describe ".merge" do
        let(:base) do
          {
            "name" => "ThingAMQP",
            "major-version" => 1,
            "minor-version" => 0,
            "revision" => 0,
            "port" => 4000,
            "domains" => [["label", "shortstr"]],
            "classes" => [
              {
                "name" => "thing",
                "id" => 77,
                "methods" => [{ "name" => "make", "id" => 10, "arguments" => [] }]
              }
            ]
          }
        end

        let(:extension) do
          {
            "name" => "ThingAMQP",
            "major-version" => 1,
            "minor-version" => 0,
            "revision" => 0,
            "port" => 4000,
            "extension" => ["thing-extension"],
            "domains" => [["tag", "shortstr"]],
            "classes" => [
              {
                "name" => "extra",
                "id" => 78,
                "methods" => [{ "name" => "poke", "id" => 10, "arguments" => [] }]
              }
            ]
          }
        end

        it "combines documents into one version, keeping declaration order" do
          merged = described_class.merge([base, extension])

          expect(merged.classes.map(&:name)).to eq(%w[thing extra])
          expect(merged.resolve_domain("tag")).to eq("shortstr")
        end

        it "keeps document order and is stable for the same document order" do
          forward = described_class.merge([base, extension])
          forward_again = described_class.merge([base, extension])

          expect(forward.classes.map(&:name)).to eq(%w[thing extra])
          expect(forward.digest).to eq(forward_again.digest)
          expect(described_class.merge([extension, base]).classes.map(&:name)).to eq(%w[extra thing])
        end

        it "reports conflicts instead of silently picking one declaration" do
          conflicting = Marshal.load(Marshal.dump(extension))
          conflicting["classes"][0]["id"] = 79

          expect {
            described_class.merge([base, extension, conflicting])
          }.to raise_error(Spec::ConflictError, /extra.*ids 78 and 79/)
        end

        it "reports a method declared twice with different arguments" do
          conflicting = Marshal.load(Marshal.dump(extension))
          conflicting["classes"] = [
            {
              "name" => "thing",
              "id" => 77,
              "methods" => [
                { "name" => "make", "id" => 10, "arguments" => [{ "type" => "bit", "name" => "flag" }] }
              ]
            }
          ]

          expect {
            described_class.merge([base, conflicting])
          }.to raise_error(Spec::ConflictError, /thing\.make/)
        end

        it "reports documents of different versions" do
          other = Marshal.load(Marshal.dump(extension))
          other["minor-version"] = 1

          expect {
            described_class.merge([base, other])
          }.to raise_error(Spec::ConflictError, /versions/)
        end

        it "reports a domain declared with two different types" do
          conflicting = Marshal.load(Marshal.dump(extension))
          conflicting["domains"] = [["label", "long"]]

          expect {
            described_class.merge([base, conflicting])
          }.to raise_error(Spec::ConflictError, /domain "label"/)
        end
      end
    end
  end
end
