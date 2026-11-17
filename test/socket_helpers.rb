require 'socket'

module TestSockets
  # UDP and TCP have separate port namespaces. Keep both sockets reserved
  # before starting a peer, and retry only collisions during allocation.
  def self.udp_tcp_pair(host = '127.0.0.1')
    10.times do |attempt|
      udp = tcp = nil
      reserved = false
      begin
        tcp = TCPServer.new(host, 0)
        udp = UDPSocket.new
        udp.bind(host, tcp.addr[1])
        reserved = true
        return udp, tcp
      rescue Errno::EADDRINUSE
        raise if attempt == 9
      ensure
        unless reserved
          udp.close if udp && !udp.closed?
          tcp.close if tcp && !tcp.closed?
        end
      end
    end
  end
end
