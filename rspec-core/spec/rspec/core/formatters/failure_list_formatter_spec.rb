require 'rspec/core/formatters/failure_list_formatter'

module RSpec::Core::Formatters
  RSpec.describe FailureListFormatter do
    include FormatterSupport

    it 'produces the expected full output' do
      output = run_example_specs_with_formatter('failures')
      expect(output).to eq(<<-EOS.gsub(/^\s+\|/, ''))
        |./spec/rspec/core/resources/formatter_specs.rb:4:is marked as pending but passes
        |./spec/rspec/core/resources/formatter_specs.rb:37:fails
        |./spec/rspec/core/resources/formatter_specs.rb:41:fails twice
        |./spec/rspec/core/resources/formatter_specs.rb:48:fails with a backtrace that has no file
        |./spec/rspec/core/resources/formatter_specs.rb:54:fails with a backtrace containing an erb file
        |./spec/rspec/core/resources/formatter_specs.rb:66:raises
      EOS
    end
  end
end
