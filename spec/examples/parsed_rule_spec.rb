require File.dirname(__FILE__) + "/../spec_helper"

module IceCube
  describe IcalParser, "parse_rule" do
    describe "extension preservation" do
      it "separates unrecognized components from the executable rule" do
        result = IcalParser.parse_rule("FREQ=DAILY;BYDAY=MO;X-ROOM=main")
        expect(result.extensions).to eq([ParsedRule::Extension.new("X-ROOM", "main")])
        expect(result.to_rule(extensions: :ignore)).to eq(Rule.daily.day(:monday))
      end

      it "keeps raw extension values verbatim, including = and case" do
        result = IcalParser.parse_rule("FREQ=DAILY;X-PAYLOAD=Token=AbC")
        expect(result.extensions.first.value).to eq("Token=AbC")
      end

      it "normalizes extension names to uppercase" do
        result = IcalParser.parse_rule("FREQ=DAILY;x-room=main")
        expect(result.extensions.first.name).to eq("X-ROOM")
      end

      it "preserves repeated extension components in order" do
        result = IcalParser.parse_rule("FREQ=DAILY;X-TAG=a;X-TAG=b")
        expect(result.extensions.map(&:value)).to eq(%w[a b])
      end

      it "matches known components case-insensitively" do
        result = IcalParser.parse_rule("freq=weekly;byday=mo,tu;interval=2")
        expect(result.extensions).to be_empty
        expect(result.to_rule).to eq(Rule.weekly.interval(2).day(:monday, :tuesday))
      end
    end

    describe "executing with unknown extensions" do
      let(:result) { IcalParser.parse_rule("FREQ=DAILY;X-ROOM=main") }

      it "rejects by default so unknown fields are never treated as no-ops" do
        expect { result.to_rule }.to raise_error(UnknownRuleExtension, /X-ROOM/)
      end

      it "rejects when explicitly asked to" do
        expect { result.to_rule(extensions: :reject) }.to raise_error(UnknownRuleExtension)
      end

      it "returns the rule when the caller explicitly ignores extensions" do
        expect(result.to_rule(extensions: :ignore)).to eq(Rule.daily)
      end

      it "rejects unknown handling options" do
        expect { result.to_rule(extensions: :drop) }.to raise_error(ArgumentError)
      end

      it "executes without an option when nothing is unrecognized" do
        expect(IcalParser.parse_rule("FREQ=DAILY").to_rule).to eq(Rule.daily)
      end
    end

    describe "round-trip" do
      it "re-emits preserved extensions so nothing is lost in transit" do
        ical = "FREQ=WEEKLY;BYDAY=MO,1FR;X-ROOM=main;X-TAG=a;X-TAG=b"
        result = IcalParser.parse_rule(ical)
        expect(result.to_ical).to eq("FREQ=WEEKLY;BYDAY=1FR,MO;X-ROOM=main;X-TAG=a;X-TAG=b")
        expect(IcalParser.parse_rule(result.to_ical)).to eq(result)
      end
    end

    describe "invalid known components" do
      it "raises for an invalid frequency even when extensions are present" do
        expect { IcalParser.parse_rule("FREQ=BOGUS;X-ROOM=main") }
          .to raise_error(ArgumentError, /Invalid rule frequency type/)
      end

      it "raises for an invalid weekday value" do
        expect { IcalParser.parse_rule("FREQ=WEEKLY;BYDAY=XX") }
          .to raise_error(ArgumentError, /Invalid day/)
      end

      it "raises for out-of-range BYSETPOS" do
        expect { IcalParser.parse_rule("FREQ=MONTHLY;BYSETPOS=999") }
          .to raise_error(ArgumentError, /Expecting number/)
      end

      it "raises for repeated known components" do
        expect { IcalParser.parse_rule("FREQ=DAILY;FREQ=WEEKLY") }
          .to raise_error(ArgumentError, /Duplicate iCal rule component: FREQ/)
      end

      it "raises for malformed components" do
        expect { IcalParser.parse_rule("FREQ=DAILY;DANGLING") }
          .to raise_error(ArgumentError, /Invalid iCal rule component/)
      end
    end

    describe "registered extensions" do
      after { IcalParser.unregister_extension("X-COUNT-LIMIT") }

      it "applies the handler once the extension is registered" do
        ical = "FREQ=DAILY;X-COUNT-LIMIT=3"

        unregistered = IcalParser.parse_rule(ical)
        expect(unregistered.extensions.map(&:name)).to eq(["X-COUNT-LIMIT"])
        expect { unregistered.to_rule }.to raise_error(UnknownRuleExtension)

        IcalParser.register_extension("X-COUNT-LIMIT") do |rule, value|
          rule.count(value.to_i)
        end

        expect(IcalParser.extension_registered?("x-count-limit")).to be(true)
        registered = IcalParser.parse_rule(ical)
        expect(registered.extensions).to be_empty
        expect(registered.to_rule).to eq(Rule.daily.count(3))
      end

      it "keeps the rule when the handler returns nil" do
        IcalParser.register_extension("X-COUNT-LIMIT") { |rule, _value| rule.hour_of_day(9) }
        expect(IcalParser.parse_rule("FREQ=DAILY;X-COUNT-LIMIT=1").to_rule)
          .to eq(Rule.daily.hour_of_day(9))
      end

      it "treats repeated registered components as duplicates" do
        IcalParser.register_extension("X-COUNT-LIMIT") { |rule, _value| rule }
        expect { IcalParser.parse_rule("FREQ=DAILY;X-COUNT-LIMIT=1;X-COUNT-LIMIT=2") }
          .to raise_error(ArgumentError, /Duplicate iCal rule component: X-COUNT-LIMIT/)
      end

      it "refuses to shadow known components" do
        expect { IcalParser.register_extension("FREQ") { |rule, _| rule } }
          .to raise_error(ArgumentError, /known RRULE component/)
      end
    end

    describe "legacy rule_from_ical" do
      it "still rejects unrecognized components" do
        expect { IcalParser.rule_from_ical("FREQ=DAILY;FAKE=23") }
          .to raise_error(ArgumentError, "Invalid rule validation type: FAKE")
      end
    end
  end
end
