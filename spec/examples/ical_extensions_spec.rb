require File.dirname(__FILE__) + "/../spec_helper"

module IceCube
  describe "iCal extension fields" do
    describe "parse result" do
      it "keeps the executable rule and unknown fields separate" do
        parsed = IcalParser.parse_rule("FREQ=DAILY;INTERVAL=2;X-FOO=bar")

        expect(parsed.rule).to eq(IceCube::Rule.daily(2))
        expect(parsed.extensions).to eq("X-FOO" => "bar")
        expect(parsed).to be_extension_fields
      end

      it "reports no extension fields for a fully understood rule" do
        parsed = IcalParser.parse_rule("FREQ=DAILY;BYDAY=MO")

        expect(parsed.extensions).to be_empty
        expect(parsed).not_to be_extension_fields
      end

      it "keeps raw parameter values unparsed" do
        parsed = IcalParser.parse_rule("FREQ=DAILY;X-NUM=007;X-STR=Some Value")

        expect(parsed.extensions).to eq("X-NUM" => "007", "X-STR" => "Some Value")
      end

      it "normalizes field names to uppercase but preserves value case" do
        parsed = IcalParser.parse_rule("freq=daily;x-foo=Bar")

        expect(parsed.rule).to eq(IceCube::Rule.daily)
        expect(parsed.extensions).to eq("X-FOO" => "Bar")
      end

      it "parses known fields case-insensitively" do
        rule = IceCube::Rule.from_ical("freq=weekly;interval=2;wkst=mo;byday=tu,-1we")

        expect(rule).to eq(IceCube::Rule.weekly(2, :monday).day(:tuesday).day_of_week(wednesday: [-1]))
      end

      it "raises on duplicate known fields" do
        expect {
          IcalParser.parse_rule("FREQ=DAILY;INTERVAL=2;INTERVAL=3")
        }.to raise_error(ArgumentError, "Duplicate iCal rule field: INTERVAL")
      end

      it "raises on duplicate extension fields, even when preserving" do
        expect {
          IceCube::Rule.from_ical("FREQ=DAILY;X-A=1;x-a=2", extensions: :preserve)
        }.to raise_error(ArgumentError, "Duplicate iCal rule field: X-A")
      end
    end

    describe "execution policy" do
      it "rejects unknown fields by default" do
        expect {
          IceCube::Rule.from_ical("FREQ=DAILY;X-FOO=bar")
        }.to raise_error(UnknownIcalFieldsError, "Invalid rule validation type: X-FOO")
      end

      it "exposes the unknown field names on the error" do
        error = nil
        begin
          IceCube::Rule.from_ical("FREQ=DAILY;X-A=1;X-B=2")
        rescue UnknownIcalFieldsError => error
        end

        expect(error).to be_a(ArgumentError)
        expect(error.fields).to eq(%w[X-A X-B])
        expect(error.message).to eq("Invalid rule validation types: X-A, X-B")
      end

      it "discards unknown fields when explicitly ignored" do
        rule = IceCube::Rule.from_ical("FREQ=DAILY;X-FOO=bar", extensions: :ignore)

        expect(rule).to eq(IceCube::Rule.daily)
        expect(rule.to_ical).to eq("FREQ=DAILY")
      end

      it "raises for an invalid extensions mode" do
        expect {
          IceCube::Rule.from_ical("FREQ=DAILY;X-FOO=bar", extensions: :bogus)
        }.to raise_error(ArgumentError, /Invalid extensions mode/)
      end

      it "still raises when a known field fails validation" do
        expect {
          IceCube::Rule.from_ical("FREQ=MONTHLY;BYSETPOS=0;X-FOO=bar", extensions: :preserve)
        }.to raise_error(/Expecting number in \[-366, -1\] or \[1, 366\]/)

        expect {
          IceCube::Rule.from_ical("FREQ=MONTHLY;BYSETPOS=0;X-FOO=bar", extensions: :ignore)
        }.to raise_error(/Expecting number in \[-366, -1\] or \[1, 366\]/)
      end
    end

    describe "preservation" do
      it "round-trips extension fields through ical" do
        rule = IceCube::Rule.from_ical("FREQ=DAILY;INTERVAL=2;X-FOO=bar;X-NUM=007", extensions: :preserve)

        expect(rule.to_ical).to eq("FREQ=DAILY;INTERVAL=2;X-FOO=bar;X-NUM=007")
        reparsed = IceCube::Rule.from_ical(rule.to_ical, extensions: :preserve)
        expect(reparsed.to_ical).to eq(rule.to_ical)
        expect(reparsed).to eq(rule)
      end

      it "round-trips extension fields through hash and yaml" do
        rule = IceCube::Rule.from_ical("FREQ=DAILY;X-FOO=bar", extensions: :preserve)

        from_hash = IceCube::Rule.from_hash(rule.to_hash)
        expect(from_hash).to eq(rule)
        expect(from_hash.ical_extensions).to eq("X-FOO" => "bar")

        from_yaml = IceCube::Rule.from_yaml(rule.to_yaml)
        expect(from_yaml).to eq(rule)
        expect(from_yaml.to_ical).to eq(rule.to_ical)
      end

      it "does not treat preserved extensions as executable" do
        rule = IceCube::Rule.from_ical("FREQ=DAILY;COUNT=2;X-FOO=bar", extensions: :preserve)
        schedule = IceCube::Schedule.new(Time.utc(2026, 1, 1))
        schedule.add_recurrence_rule(rule)

        expect(schedule.occurrences(Time.utc(2026, 2, 1))).to eq([
          Time.utc(2026, 1, 1), Time.utc(2026, 1, 2)
        ])
      end

      it "round-trips extension fields through a schedule" do
        ical = "DTSTART:20130314T201500Z\nRRULE:FREQ=WEEKLY;BYDAY=TH;X-FOO=bar"
        schedule = IceCube::Schedule.from_ical(ical, extensions: :preserve)

        expect(schedule.to_ical).to include("RRULE:FREQ=WEEKLY;BYDAY=TH;X-FOO=bar")
      end

      it "rejects unknown fields in schedules by default" do
        ical = "DTSTART:20130314T201500Z\nRRULE:FREQ=WEEKLY;X-FOO=bar"

        expect {
          IceCube::Schedule.from_ical(ical)
        }.to raise_error(UnknownIcalFieldsError)
      end
    end

    describe "registered extensions" do
      after do
        IcalParser.unregister_extension("X-COUNT-CAP")
      end

      it "applies a handler once the extension is registered" do
        expect {
          IceCube::Rule.from_ical("FREQ=DAILY;X-COUNT-CAP=5")
        }.to raise_error(UnknownIcalFieldsError)

        IcalParser.register_extension("X-COUNT-CAP") do |rule, value|
          rule.count(value.to_i)
        end

        rule = IceCube::Rule.from_ical("FREQ=DAILY;X-COUNT-CAP=5")
        expect(rule).to eq(IceCube::Rule.daily.count(5))
      end

      it "no longer reports a registered field as an extension" do
        IcalParser.register_extension("x-count-cap") { |rule, value| rule.count(value.to_i) }

        parsed = IcalParser.parse_rule("FREQ=DAILY;X-COUNT-CAP=3")
        expect(parsed).not_to be_extension_fields
        expect(parsed.rule).to eq(IceCube::Rule.daily.count(3))
      end
    end
  end
end
