module GraphQL::Batch
  # Outcome of a single key in a Loader#load_many_settled result.
  # The state is recorded explicitly, so a fulfilled nil value is
  # never confused with a rejection.
  class SettledResult
    def self.fulfilled(value)
      new(true, value)
    end

    def self.rejected(reason)
      new(false, reason)
    end

    def initialize(fulfilled, payload)
      @fulfilled = fulfilled
      @payload = payload
    end

    def fulfilled?
      @fulfilled
    end

    def rejected?
      !@fulfilled
    end

    def value
      @payload if @fulfilled
    end

    def reason
      @payload unless @fulfilled
    end
  end
end
