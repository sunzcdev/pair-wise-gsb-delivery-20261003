# encoding: utf-8
# frozen_string_literal: true
Warden::Strategies.add(:factor_two) do
  def authenticate!
    request.env['warden.spec.strategies'] ||= []
    request.env['warden.spec.strategies'] << :factor_two

    if params['factor_two'] == 'fail'
      fail!('Second factor failed')
    else
      success!(FakeSubject.new(params['factor_two_id'].to_i), 'Second factor accepted')
    end
  end
end
