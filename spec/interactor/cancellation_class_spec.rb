require "spec_helper"

module Interactor
  describe Cancellation do
    describe "#cancel" do
      it "records a cancellation reason" do
        cancellation = described_class.new

        expect {
          cancellation.cancel("request abandoned")
        }.to change(cancellation, :cancelled?).from(false).to(true)

        expect(cancellation.reason).to eq("request abandoned")
      end

      it "supports a bang alias" do
        cancellation = described_class.new

        cancellation.cancel!("timeout")

        expect(cancellation).to be_cancelled
        expect(cancellation.reason).to eq("timeout")
      end

      it "preserves the previous reason when no reason is given" do
        cancellation = described_class.new
        cancellation.cancel("first reason")

        cancellation.cancel

        expect(cancellation.reason).to eq("first reason")
      end
    end

    describe ".cancel" do
      it "builds a cancelled signal" do
        cancellation = described_class.cancel("closed")

        expect(cancellation).to be_cancelled
        expect(cancellation.reason).to eq("closed")
      end
    end

    describe "#canceled?" do
      it "supports American spelling" do
        cancellation = described_class.new

        expect(cancellation).not_to be_canceled

        cancellation.cancel

        expect(cancellation).to be_canceled
      end
    end
  end
end
