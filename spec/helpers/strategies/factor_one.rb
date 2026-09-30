# encoding: utf-8
# frozen_string_literal: true
Warden::Strategies.add(:factor_one) do
  def authenticate!
    request.env['warden.spec.strategies'] ||= []
    request.env['warden.spec.strategies'] << :factor_one

    if params['factor_one'] == 'fail'
      fail!('First factor failed')
    else
      success!(FakeSubject.new(params['factor_one_id'].to_i), 'First factor accepted')
    end
  end
end
