# encoding: utf-8
# frozen_string_literal: true

RSpec.describe Warden::Challenge do
  describe "serialization" do
    it "serializes a bare scheme" do
      expect(Warden::Challenge.new('Bearer').to_s).to eq('Bearer')
    end

    it "accepts symbols as scheme" do
      expect(Warden::Challenge.new(:Basic).to_s).to eq('Basic')
    end

    it "serializes a scheme with a token68" do
      expect(Warden::Challenge.new('Bearer', 'abcDEF-._~+/==').to_s).to eq('Bearer abcDEF-._~+/==')
    end

    it "serializes auth-params leaving token values bare" do
      expect(Warden::Challenge.new('Basic', :realm => 'Restricted').to_s).to eq('Basic realm=Restricted')
    end

    it "serializes multiple auth-params as a comma separated list" do
      challenge = Warden::Challenge.new('Newauth', :realm => 'apps', :type => '1')
      expect(challenge.to_s).to eq('Newauth realm=apps, type=1')
    end

    it "quotes values containing commas so they cannot break challenge boundaries" do
      challenge = Warden::Challenge.new('Basic', :realm => 'foo, bar')
      expect(challenge.to_s).to eq('Basic realm="foo, bar"')
    end

    it "escapes double quotes and backslashes inside quoted values" do
      challenge = Warden::Challenge.new('Basic', :title => 'Login to "my" \\ apps')
      expect(challenge.to_s).to eq('Basic title="Login to \\"my\\" \\\\ apps"')
    end

    it "compares challenges by their serialized form" do
      expect(Warden::Challenge.new('Basic', :realm => 'a')).to eq(Warden::Challenge.new('Basic', :realm => 'a'))
      expect(Warden::Challenge.new('Basic', :realm => 'a')).not_to eq(Warden::Challenge.new('Basic', :realm => 'b'))
    end
  end

  describe "validation" do
    it "rejects a scheme that is not a token" do
      expect { Warden::Challenge.new('Bad Scheme') }.to raise_error(ArgumentError)
      expect { Warden::Challenge.new('Basic"') }.to raise_error(ArgumentError)
      expect { Warden::Challenge.new('') }.to raise_error(ArgumentError)
    end

    it "rejects an invalid token68" do
      expect { Warden::Challenge.new('Bearer', 'not valid!') }.to raise_error(ArgumentError)
      expect { Warden::Challenge.new('Bearer', '') }.to raise_error(ArgumentError)
    end

    it "rejects a parameter name that is not a token" do
      expect { Warden::Challenge.new('Basic', 'bad name' => 'x') }.to raise_error(ArgumentError)
      expect { Warden::Challenge.new('Basic', '' => 'x') }.to raise_error(ArgumentError)
    end

    it "rejects control characters in parameter values" do
      expect { Warden::Challenge.new('Basic', :realm => "a\r\nb") }.to raise_error(ArgumentError)
      expect { Warden::Challenge.new('Basic', :realm => "a\0b") }.to raise_error(ArgumentError)
    end

    it "rejects unsupported parameter input" do
      expect { Warden::Challenge.new('Basic', 42) }.to raise_error(ArgumentError)
      expect { Warden::Challenge.new('Basic', [:realm]) }.to raise_error(ArgumentError)
    end
  end
end
