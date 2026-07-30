# frozen_string_literal: true

require 'socket'
require_relative 'http'

module SlotMachine
  # Proxy forwards requests on a fixed public port to whichever slot is live.
  #
  # The public port belongs to the orchestrator, not to any app process. That is
  # the whole reason a deploy can replace the process behind it without the port
  # ever going away, and it is why the port is bound at startup rather than when
  # something first becomes live: an operator whose first deploy failed still
  # needs the port to answer, and to answer with something they can act on.
  class Proxy
    def initialize(port, name:)
      @port = port
      @name = name
      @target = nil
      @mutex = Mutex.new
    end

    attr_reader :port

    def start
      listener = HTTP.bind(@port)
      @server = HTTP::Server.new(listener) { |sock, request| handle(sock, request) }.start
      self
    rescue Errno::EADDRINUSE => e
      warn "proxy: cannot bind #{@name} port #{@port}: #{e.message}"
      nil
    end

    def stop
      @server&.stop
    end

    def target=(value)
      @mutex.synchronize { @target = value }
    end

    def target
      @mutex.synchronize { @target }
    end

    def listening?
      !@server.nil?
    end

    private

    def handle(sock, request)
      upstream_port = target
      if upstream_port.nil?
        # Nothing is live. 503 rather than a refused connection: a caller cannot
        # tell a refused connection from a machine that has gone away, and
        # dropping the listener would hand the port to whatever wanted it next.
        HTTP.write_response(sock, 503, "no live slot\n", content_type: 'text/plain')
        return
      end

      forward(sock, request, upstream_port)
    end

    def forward(sock, request, upstream_port)
      upstream = TCPSocket.new('127.0.0.1', upstream_port)

      # Rebuild the head, forcing Connection: close so the exchange has an
      # unambiguous end. Hop-by-hop headers are ours to drop, not to relay.
      head = +request.request_line
      request.headers.each do |name, value|
        next if %w[connection keep-alive proxy-connection te upgrade transfer-encoding].include?(name)

        head << "#{name}: #{value}\r\n"
      end
      head << "Connection: close\r\n\r\n"

      upstream.write(head)
      upstream.write(request.body) if request.body
      upstream.flush

      # Stream the response straight back. Copying in chunks rather than reading
      # it all first keeps a streaming response (server-sent events, a long
      # download) from being buffered until it completes.
      IO.copy_stream(upstream, sock)
    rescue Errno::ECONNREFUSED, Errno::ECONNRESET, Errno::EPIPE => e
      # The live process died between the target being set and this request.
      warn "proxy: upstream #{upstream_port}: #{e.class}"
      begin
        HTTP.write_response(sock, 502, "bad gateway\n", content_type: 'text/plain')
      rescue StandardError
        nil
      end
    ensure
      begin
        upstream&.close
      rescue StandardError
        nil
      end
    end
  end
end
