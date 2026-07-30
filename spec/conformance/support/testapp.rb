#!/usr/bin/env ruby
# frozen_string_literal: true

# A minimal application for exercising an orchestrator.
#
# It implements only what the spec requires of an app, and nothing else:
#
#   - binds the port given in PORT for public traffic
#   - binds the port given in INTERNAL_PORT for the health endpoint
#   - answers the health endpoint with 200 once it is ready
#   - exits cleanly on SIGTERM
#
# It also serves the contents of a VERSION file in its working directory, which
# is what lets a test prove that traffic reached the slot it expected rather than
# inferring it from the orchestrator's own status report.
#
# The control endpoints exist so a test can provoke the failure modes the spec's
# scenarios describe: crashing after promotion, and refusing to exit on SIGTERM.
# They live on the internal port, which the orchestrator must not expose.

require 'socket'

BOOT_DELAY = Integer(ENV.fetch('APP_BOOT_DELAY', '0'))
START_UNHEALTHY = ENV.fetch('APP_START_UNHEALTHY', '') == '1'
HEALTH_PATH = ENV.fetch('APP_HEALTH_PATH', '/healthz')

def port_from(name)
  value = ENV[name]
  if value.nil? || value.empty?
    warn "testapp: #{name} is not set; the orchestrator must provide it"
    exit 64
  end
  Integer(value)
end

$healthy = !START_UNHEALTHY
$version = File.exist?('VERSION') ? File.read('VERSION').strip : 'no-version-file'

# ---------------------------------------------------------------------------
# A very small HTTP server.
#
# Hand-rolled because an app that needs a web framework to answer two routes
# would say more about Ruby packaging than about the orchestrator under test.
# ---------------------------------------------------------------------------

def read_request(sock)
  request_line = sock.gets
  return nil if request_line.nil?

  method, path, = request_line.split
  # Drain the headers so the client sees a complete exchange, and note the body
  # length so a POST does not leave bytes in the socket.
  length = 0
  while (line = sock.gets)
    break if line == "\r\n" || line == "\n"

    name, value = line.split(':', 2)
    length = value.to_i if name&.downcase == 'content-length'
  end
  sock.read(length) if length.positive?

  [method, path]
end

def respond(sock, status, body)
  reason = { 200 => 'OK', 404 => 'Not Found', 503 => 'Service Unavailable' }.fetch(status, 'Status')
  sock.print(
    "HTTP/1.1 #{status} #{reason}\r\n" \
    "Content-Type: text/plain\r\n" \
    "Content-Length: #{body.bytesize}\r\n" \
    "Connection: close\r\n" \
    "\r\n#{body}"
  )
end

def serve(port, name)
  server = begin
    TCPServer.new('127.0.0.1', port)
  rescue Errno::EADDRINUSE => e
    warn "testapp: cannot bind #{name} port #{port}: #{e}"
    exit 65
  end

  Thread.new do
    loop do
      sock = server.accept
      Thread.new(sock) do |connection|
        begin
          method, path = read_request(connection)
          status, body = yield(method, path) if method
          respond(connection, status || 404, body || '') if method
        rescue StandardError
          # A broken connection is not this app's problem.
        ensure
          connection.close rescue nil
        end
      end
    end
  end
end

# ---------------------------------------------------------------------------

# A slow boot is how a test drives the health-check timeout: the orchestrator
# must give up on an app that is not ready in time, without disturbing whatever
# is already live.
sleep(BOOT_DELAY) if BOOT_DELAY.positive?

public_port = port_from('PORT')
internal_port = port_from('INTERNAL_PORT')

serve(public_port, 'public') do |_method, _path|
  [200, "version=#{$version}\n"]
end

serve(internal_port, 'internal') do |method, path|
  if method == 'GET' && path == HEALTH_PATH
    $healthy ? [200, "ok\n"] : [503, "unhealthy\n"]
  elsif method == 'POST' && path == '/control/unhealthy'
    $healthy = false
    [200, "now unhealthy\n"]
  elsif method == 'POST' && path == '/control/healthy'
    $healthy = true
    [200, "now healthy\n"]
  elsif method == 'POST' && path == '/control/hang'
    # Stop honouring SIGTERM, so only SIGKILL ends this process. This is what
    # the drain-timeout scenario needs.
    Signal.trap('TERM') { nil }
    [200, "now ignoring SIGTERM\n"]
  elsif method == 'POST' && path == '/control/crash'
    # Die without draining, to imitate a process that falls over after being
    # promoted. Answering first keeps the caller from seeing a broken socket.
    Thread.new do
      sleep 0.05
      exit!(1)
    end
    [200, "crashing\n"]
  else
    [404, "not found\n"]
  end
end

# The spec requires a graceful exit on SIGTERM. Anything the orchestrator has to
# SIGKILL is a bug in the app, not in the orchestrator — except when
# /control/hang has deliberately made it one.
Signal.trap('TERM') { exit(0) }
Signal.trap('INT') { exit(0) }

puts "testapp: version=#{$version} public=#{public_port} internal=#{internal_port}"
$stdout.flush
sleep
