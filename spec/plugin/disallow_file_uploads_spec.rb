require_relative "../spec_helper"

begin
  require_relative '../../lib/roda/plugins/disallow_file_uploads'
rescue LoadError
  warn "Rack #{Rack.release} used, skipping disallow_file_uploads plugin test"  
else
describe "disallow_file_uploads plugin" do 
  it "disallows the uploading of files" do
    app do |r|
      r.params['foo'][:tempfile].read
    end

    request_body = rack_input("------WebKitFormBoundarymwHIM9XjTTVHn3YP\r\nContent-Disposition: form-data; name=\"foo\"; filename=\"bar.txt\"\r\nContent-Type: text/plain\r\n\r\nfoo\n\r\n------WebKitFormBoundarymwHIM9XjTTVHn3YP--\r\n")

    h = {
      'rack.input'=>request_body,
      'CONTENT_TYPE'=>'multipart/form-data; boundary=----WebKitFormBoundarymwHIM9XjTTVHn3YP',
      'CONTENT_LENGTH'=>'184',
      'REQUEST_METHOD'=>'POST'
    }
    body(h.dup).must_equal "foo\n"
    app.plugin :disallow_file_uploads
    request_body.rewind
    proc{body(h.dup)}.must_raise Roda::RodaPlugins::DisallowFileUploads::Error
  end
end
end
