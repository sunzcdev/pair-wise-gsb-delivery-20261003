# encoding: utf-8
# frozen_string_literal: true
# A strategy whose outcome is driven by the request environment. This lets a
# single, cached strategy instance run several rounds within one request
# (success, then failure, then redirect, ...) just by changing the "mode".
Warden::Strategies.add(:switchable) do
  def authenticate!
    request.env["warden.spec.strategies"] ||= []
    request.env["warden.spec.strategies"] << :switchable
    request.env["warden.spec.switchable_runs"] = runs + 1

    case request.env["warden.spec.switchable_mode"]
    when :success
      success!("Switchable User", "Welcome back")
    when :redirect
      redirect!("/sign-in", {}, :message => "Please sign in again")
    when :custom
      custom!([418, {"X-Custom" => "yes"}, ["Teapot"]])
    when :pass
      pass
    else
      fail!("Credentials rejected")
    end
  end

  def runs
    request.env["warden.spec.switchable_runs"] || 0
  end
end
