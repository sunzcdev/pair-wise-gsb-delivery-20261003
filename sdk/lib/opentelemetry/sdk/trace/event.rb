# frozen_string_literal: true

# Copyright The OpenTelemetry Authors
#
# SPDX-License-Identifier: Apache-2.0

module OpenTelemetry
  module SDK
    module Trace
      # A text annotation with a set of attributes and a timestamp for export.
      #
      # Field types are as follows:
      #  name: String
      #  attributes: frozen Hash{String => String, Numeric, Boolean, Array<String, Numeric, Boolean>}
      #  timestamp: Integer nanoseconds since Epoch
      #  preferred: Boolean (whether the event is marked for retention, see
      #    {OpenTelemetry::SDK::Trace::SpanLimits#retention_policy})
      Event = Struct.new(:name, :attributes, :timestamp, :preferred) do
        # Returns true if the event was marked as preferred for retention.
        #
        # @return [Boolean]
        def preferred?
          preferred ? true : false
        end
      end
    end
  end
end
