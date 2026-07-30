# frozen_string_literal: true

require 'socket'

# A very small HTTP/1.1 server, enough for the orchestrator's API and its proxy.
#
# Hand-rolled because Ruby 3 dropped WEBrick from the default gems, and requiring
# a web framework to answer four routes would make this implementation a
# statement about Ruby packaging rather than about the spec. Everything here is
# ordinary blocking IO with a thread per connection; an orchestrator handles a
# handful of requests a day.
module SlotMachine
  # Request is a parsed HTTP request. `raw` keeps the original head so the proxy
  # can forward it without reconstructing anything it did not understand.
  Request = Struct.new(:method, :path, :headers, :body, :request_line, keyword_init: true)

  module HTTP
    REASONS = {
      200 => 'OK', 204 => 'No Content', 400 => 'Bad Request', 404 => 'Not Found',
      409 => 'Conflict', 422 => 'Unprocessable Entity', 500 => 'Internal Server Error',
      502 => 'Bad Gateway', 503 => 'Service Unavailable'
    }.freeze

    # read_request parses one request from a socket, or returns nil at EOF.
    def self.read_request(sock)
      request_line = sock.gets
      return nil if request_line.nil?

      method, path, = request_line.split
      return nil if method.nil? || path.nil?

      headers = {}
      while (line = sock.gets)
        break if line == "\r\n" || line == "\n"

        name, value = line.split(':', 2)
        headers[name.strip.downcase] = value.to_s.strip if name
      end

      length = headers['content-length'].to_i
      body = length.positive? ? sock.read(length) : nil

      Request.new(method: method, path: path, headers: headers, body: body,
                  request_line: request_line)
    end

    def self.write_response(sock, status, body, content_type: 'application/json')
      body = body.to_s
      sock.print(
        "HTTP/1.1 #{status} #{REASONS.fetch(status, 'Status')}\r\n" \
        "Content-Type: #{content_type}\r\n" \
        "Content-Length: #{body.bytesize}\r\n" \
        "Connection: close\r\n" \
        "\r\n#{body}"
      )
    end

    # Server accepts connections on an already-bound listener.
    #
    # The listener is passed in rather than created here so the caller can bind
    # its ports before doing any other work — see the note in slot_machine.rb
    # about why binding early matters.
    class Server
      def initialize(listener, &handler)
        @listener = listener
        @handler = handler
      end

      def start
        @thread = Thread.new do
          loop do
            sock = begin
              @listener.accept
            rescue IOError, Errno::EBADF
              break # listener closed
            end
            Thread.new(sock) { |connection| serve(connection) }
          end
        end
        self
      end

      def stop
        @listener.close
      rescue IOError
        nil
      end

      private

      def serve(sock)
        request = HTTP.read_request(sock)
        @handler.call(sock, request) if request
      rescue Errno::EPIPE, Errno::ECONNRESET
        nil
      rescue StandardError => e
        warn "http: #{e.class}: #{e.message}"
      ensure
        begin
          sock.close
        rescue StandardError
          nil
        end
      end
    end

    # bind returns a listener on 127.0.0.1:port, retrying briefly.
    #
    # The retry is for restarts: a predecessor that is still draining may hold
    # the port for a moment, and failing permanently there turns an ordinary
    # restart into an outage that needs a human.
    def self.bind(port, timeout: 3)
      deadline = Time.now + timeout
      loop do
        begin
          return TCPServer.new('127.0.0.1', port)
        rescue Errno::EADDRINUSE
          raise if Time.now > deadline

          sleep 0.05
        end
      end
    end

    # free_port asks the kernel for an unused port.
    #
    # Inherently a race: the port is released before the caller binds it. The
    # caller must be prepared for the bind to fail.
    def self.free_port
      server = TCPServer.new('127.0.0.1', 0)
      port = server.addr[1]
      server.close
      port
    end
  end
end
