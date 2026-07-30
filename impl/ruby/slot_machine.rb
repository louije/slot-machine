#!/usr/bin/env ruby
# frozen_string_literal: true

# A second implementation of docs/orchestrator-spec.md.
#
# Its purpose is not to be used. It exists so that the spec is a specification
# rather than a description of one program: a suite that has only ever run
# against a single implementation encodes that implementation's assumptions
# invisibly, and the only way to find them is to write another one.
#
# It implements the spec's core and nothing else — no agent, no chat, no deploy
# gate, no machine branch, no migration checks. Those are slot-machine's
# extensions, and a conformant orchestrator must not need them.
#
# Usage:
#
#   ruby slot_machine.rb --config <contract.json> --repo <dir> --data <dir> --port <n>

require 'json'

require_relative 'lib/contract'
require_relative 'lib/http'
require_relative 'lib/orchestrator'

module SlotMachine
  class CLI
    def self.run(argv)
      options = parse(argv)

      begin
        contract = Contract.load(options[:config])
      rescue Errno::ENOENT
        abort "error: cannot read #{options[:config]}"
      rescue ArgumentError => e
        abort "error: #{e.message}"
      end

      # Bind the API port before anything else happens.
      #
      # Not cosmetic: booting a slot allocates ports from the ephemeral range, so
      # until this listener exists the API port is merely unclaimed and an app
      # process can be handed it. Reserving it up front closes that window.
      # Serving on it happens last, so that a reachable API still means startup
      # has finished.
      api_listener = begin
        HTTP.bind(options[:port])
      rescue Errno::EADDRINUSE
        abort "error: cannot listen on port #{options[:port]}; is another instance running?"
      end

      orchestrator = Orchestrator.new(contract: contract,
                                      repo_dir: options[:repo],
                                      data_dir: options[:data])

      # The public port answers before anything is live, with 503. An operator
      # whose first deploy failed still needs a reachable port.
      orchestrator.start_proxies

      # Deploy HEAD, so a fresh start serves something.
      head = `git -C #{options[:repo]} rev-parse HEAD 2>/dev/null`.strip
      unless head.empty?
        response, = orchestrator.deploy(head)
        if response[:success]
          warn "deployed #{head[0, 8]} to #{response[:slot]}"
        else
          warn "startup deploy of #{head[0, 8]} failed: #{response[:error]}"
        end
      end

      api = HTTP::Server.new(api_listener) do |sock, request|
        handle_api(sock, request, orchestrator)
      end.start

      %w[TERM INT].each do |signal|
        Signal.trap(signal) do
          # Signal handlers cannot safely do much; hand off to a thread.
          Thread.new do
            api.stop
            orchestrator.stop
            exit(0)
          end
        end
      end

      warn "slot-machine (ruby) listening on port #{options[:port]}"
      api.join
    end

    def self.handle_api(sock, request, orchestrator)
      case [request.method, request.path]
      when %w[GET /]
        # Readiness. The conformance harness waits on this to know the
        # orchestrator is up, and it is the only endpoint that answers before
        # anything has been deployed.
        HTTP.write_response(sock, 200, JSON.generate(status: 'ok'))
      when %w[POST /deploy]
        payload = begin
          JSON.parse(request.body.to_s)
        rescue JSON::ParserError
          {}
        end
        body, code = orchestrator.deploy(payload['commit'])
        HTTP.write_response(sock, code, JSON.generate(body))
      when %w[POST /rollback]
        body, code = orchestrator.rollback
        HTTP.write_response(sock, code, JSON.generate(body))
      when %w[GET /status]
        HTTP.write_response(sock, 200, JSON.generate(orchestrator.status))
      else
        HTTP.write_response(sock, 404, JSON.generate(error: 'not found'))
      end
    end

    def self.parse(argv)
      options = {}
      argv.each_slice(2) do |flag, value|
        case flag
        when '--config' then options[:config] = value
        when '--repo'   then options[:repo] = value
        when '--data'   then options[:data] = value
        when '--port'   then options[:port] = Integer(value)
        when '--no-proxy' then nil # accepted and ignored, for harness parity
        else abort "error: unknown flag #{flag}"
        end
      end

      missing = %i[config repo data port].reject { |k| options[k] }
      abort "error: missing #{missing.map { |m| "--#{m}" }.join(', ')}" unless missing.empty?

      options
    end
  end
end

# Server#join is needed by the CLI; add it here rather than complicating the
# server with a lifecycle it does not otherwise have.
module SlotMachine
  module HTTP
    class Server
      def join
        @thread.join
      end
    end
  end
end

SlotMachine::CLI.run(ARGV) if $PROGRAM_NAME == __FILE__
