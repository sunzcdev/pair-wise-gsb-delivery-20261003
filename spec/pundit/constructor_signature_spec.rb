# frozen_string_literal: true

require "spec_helper"

RSpec.describe Pundit::ConstructorSignature do
  def policy_class(&block)
    Class.new(&block)
  end

  describe ".verify!" do
    it "does not raise for a constructor with two fixed parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor with optional parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record = nil); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor with a rest parameter" do
      expect do
        described_class.verify!(policy_class { def initialize(*arguments); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor with one fixed and a rest parameter" do
      expect do
        described_class.verify!(policy_class { def initialize(user, *arguments); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor with fixed and optional parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record = nil, extra = nil); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor with anonymous rest and keyword rest parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(*, **); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor accepting optional keywords" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record, extra: nil); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor with a keyword rest parameter" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record, **options); end })
      end.not_to raise_error
    end

    it "does not raise for a constructor accepting a block parameter" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record, &block); end })
      end.not_to raise_error
    end

    it "does not raise when the class defines a compatible custom `.new`" do
      expect do
        described_class.verify!(policy_class { def self.new(user, record); super; end })
      end.not_to raise_error
    end

    it "raises for a constructor with no parameters" do
      expect do
        described_class.verify!(policy_class)
      end.to raise_error(Pundit::InvalidConstructorError, /accepts no parameters/)
    end

    it "raises for a constructor with too few fixed parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(user); end })
      end.to raise_error(Pundit::InvalidConstructorError, /#initialize with 2 positional arguments/)
    end

    it "raises for a constructor with too many fixed parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record, extra); end })
      end.to raise_error(Pundit::InvalidConstructorError, /constructor accepts user, record, extra/)
    end

    it "raises with a readable signature for required keyword parameters" do
      expect do
        described_class.verify!(policy_class { def initialize(user, record, required:); end })
      end.to raise_error(Pundit::InvalidConstructorError, /required keyword required/)
    end

    it "raises even when positional arguments fit but a keyword is required" do
      expect do
        described_class.verify!(policy_class { def initialize(user = nil, record = nil, required:); end })
      end.to raise_error(Pundit::InvalidConstructorError, /required keyword required/)
    end

    it "raises when the class defines an incompatible custom `.new`" do
      expect do
        described_class.verify!(policy_class { def self.new(user); super; end })
      end.to raise_error(Pundit::InvalidConstructorError, /\.new with 2 positional arguments/)
    end

    it "mentions which class has the invalid constructor" do
      expect do
        described_class.verify!(RequiredKeywordPolicy)
      end.to raise_error(Pundit::InvalidConstructorError, /#<RequiredKeywordPolicy>/)
    end

    it "does not invoke the constructor while checking it" do
      expect do
        described_class.verify!(BadArityPolicy)
      end.to raise_error(Pundit::InvalidConstructorError)

      expect(BadArityPolicy.attempts).to eq(0)
    end
  end
end
