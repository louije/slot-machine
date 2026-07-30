# frozen_string_literal: true

# Test harness for the orchestrator conformance suite.
#
# Everything here is written against docs/orchestrator-spec.md and nothing else.
# It must never reach into an implementation's internals — no slot directory
# names, no symlinks, no private files — because the spec does not define them
# and an implementation is free to do something different.
#
# The only thing it knows about a specific implementation is how to start it, and
# that lives behind an adapter script rather than in here.

require 'fileutils'
require 'json'
require 'net/http'
require 'open3'
require 'socket'
require 'tmpdir'

module Conformance
  # How long to wait for an implementation's API to come up. Generous, because
  # an implementation is permitted to finish its startup work before it answers.
  START_TIMEOUT = 30

  class Error < StandardError; end

  # Adapter locates the executable that knows how to start one implementation.
  #
  # The contract is deliberately tiny. An adapter is an executable invoked as
  #
  #   adapter --config <path> --repo <dir> --data <dir> --port <n>
  #
  # which must run the orchestrator in the foreground until it is signalled. Any
  # implementation can satisfy that with a three-line shell script, which is the
  # point: the spec describes an HTTP contract, not a command line.
  module Adapter
    def self.path
      from_env = ENV['ORCHESTRATOR_ADAPTER']
      raise Error, 'set ORCHESTRATOR_ADAPTER to an adapter script' if from_env.nil? || from_env.empty?

      full = File.expand_path(from_env)
      raise Error, "adapter not found: #{full}" unless File.executable?(full)

      full
    end

    def self.name
      File.basename(path)
    end
  end

  # Ports reserves TCP ports and holds them until the moment before launch, so
  # that parallel runs do not collide.
  class Ports
    attr_reader :api, :app, :internal

    def initialize
      @listeners = 3.times.map { TCPServer.new('127.0.0.1', 0) }
      @api, @app, @internal = @listeners.map { |l| l.addr[1] }
    end

    def release
      @listeners.each { |l| l.close rescue nil }
      @listeners = []
    end
  end

  # Repo builds a git repository containing the test app, with several commits
  # whose behaviour differs. The orchestrator is expected to deploy commits from
  # it; how it gets a working tree out of a commit is its own business.
  class Repo
    attr_reader :dir, :healthy, :second, :third, :unhealthy, :slow, :hangs

    def initialize(root)
      @dir = File.join(root, 'repo')
      FileUtils.mkdir_p(@dir)

      FileUtils.cp(File.join(__dir__, 'testapp.rb'), File.join(@dir, 'testapp.rb'))

      git('init', '--quiet')
      git('checkout', '--quiet', '-b', 'main')
      git('config', 'user.email', 'conformance@example.test')
      git('config', 'user.name', 'conformance')
      git('config', 'commit.gpgsign', 'false')

      @healthy   = commit('v1', 'ruby testapp.rb', 'a healthy app')
      @second    = commit('v2', 'ruby testapp.rb', 'a second healthy version')
      @third     = commit('v3', 'ruby testapp.rb', 'a third healthy version')
      @unhealthy = commit('bad', 'APP_START_UNHEALTHY=1 ruby testapp.rb', 'an app whose health endpoint refuses')
      @slow      = commit('slow', 'APP_BOOT_DELAY=5 ruby testapp.rb', 'an app that takes too long to boot')

      # HEAD must be a healthy version: an implementation is allowed to deploy
      # HEAD at startup, and a test should not depend on whether it does.
      @hangs = commit('v4', 'ruby testapp.rb', 'a healthy app at HEAD')
    end

    def git(*args)
      out, status = Open3.capture2e('git', *args, chdir: @dir)
      raise Error, "git #{args.join(' ')} failed: #{out}" unless status.success?

      out.strip
    end

    private

    # Each commit writes a VERSION file and a start script. VERSION is what the
    # app serves, so a test can tell which commit is actually handling traffic.
    def commit(version, command, message)
      File.write(File.join(@dir, 'VERSION'), "#{version}\n")
      File.write(File.join(@dir, 'start.sh'), "#!/bin/sh\nexec #{command}\n")
      File.chmod(0o755, File.join(@dir, 'start.sh'))
      git('add', '-A')
      git('commit', '--quiet', '-m', message)
      git('rev-parse', 'HEAD')
    end
  end

  # Orchestrator runs one implementation under test.
  class Orchestrator
    attr_reader :ports, :repo, :data_dir

    def initialize(contract: {}, drain_timeout_ms: 2_000, health_timeout_ms: 8_000)
      @root = Dir.mktmpdir('conformance')
      @ports = Ports.new
      @repo = Repo.new(@root)
      @data_dir = File.join(@root, 'data')
      FileUtils.mkdir_p(@data_dir)

      # The app contract, exactly as the spec defines it.
      @contract = {
        'start_command' => './start.sh',
        'port' => @ports.app,
        'internal_port' => @ports.internal,
        'health_endpoint' => '/healthz',
        'health_timeout_ms' => health_timeout_ms,
        'drain_timeout_ms' => drain_timeout_ms
      }.merge(contract)

      @contract_path = File.join(@root, 'app.contract.json')
      File.write(@contract_path, JSON.pretty_generate(@contract))
    end

    def start
      @ports.release
      @pid = spawn(
        Adapter.path,
        '--config', @contract_path,
        '--repo', @repo.dir,
        '--data', @data_dir,
        '--port', @ports.api.to_s,
        pgroup: true,
        out: $stdout,
        err: $stderr
      )
      wait_for_api
      self
    end

    def stop
      return if @pid.nil?

      # Signal the group: an adapter is usually a shell that exec's, but it may
      # not be, and an orphaned orchestrator would keep its ports.
      begin
        Process.kill('-TERM', Process.getpgid(@pid))
      rescue Errno::ESRCH
        return
      end

      deadline = Time.now + 10
      loop do
        break if Process.waitpid(@pid, Process::WNOHANG)
        raise Error, 'orchestrator did not exit on SIGTERM' if Time.now > deadline

        sleep 0.05
      end
    rescue Errno::ECHILD
      nil
    ensure
      @pid = nil
      FileUtils.remove_entry(@root) rescue nil
    end

    # --- the spec's interface ------------------------------------------------

    def deploy(commit)
      api_post('/deploy', { commit: commit })
    end

    def deploy!(commit)
      response, code = deploy(commit)
      unless response['success']
        raise Error, "deploy of #{commit[0, 8]} failed (HTTP #{code}): #{response.inspect}"
      end

      response
    end

    def rollback
      api_post('/rollback', nil)
    end

    def rollback!
      response, code = rollback
      raise Error, "rollback failed (HTTP #{code}): #{response.inspect}" unless response['success']

      response
    end

    def status
      response, = api_get('/status')
      response
    end

    # --- talking to the app through the orchestrator's proxy ----------------

    # public_version returns the VERSION the app currently serving traffic
    # reports, or nil if the public port answered something other than 200.
    #
    # This is how a test verifies routing: not by trusting the orchestrator's
    # own account of what is live, but by asking the app.
    def public_version
      code, body = http_get(@ports.app, '/')
      return nil unless code == 200

      body[/version=(\S+)/, 1]
    end

    def public_code
      code, = http_get(@ports.app, '/')
      code
    end

    def app_control(action)
      http_post(@ports.internal, "/control/#{action}")
    end

    def wait_for_public_version(version, timeout: 15)
      deadline = Time.now + timeout
      loop do
        return true if public_version == version
        return false if Time.now > deadline

        sleep 0.1
      end
    end

    def wait_for_public_code(want, timeout: 15)
      deadline = Time.now + timeout
      last = nil
      loop do
        last = begin
          public_code
        rescue StandardError => e
          e.class.name
        end
        return true if last == want
        return false if Time.now > deadline

        sleep 0.1
      end
    end

    private

    def wait_for_api
      deadline = Time.now + START_TIMEOUT
      loop do
        begin
          _, code = api_get('/')
          return if code == 200
        rescue StandardError
          # not up yet
        end
        raise Error, "#{Adapter.name} did not answer GET / on its API port within #{START_TIMEOUT}s" if Time.now > deadline

        sleep 0.1
      end
    end

    def api_get(path)
      code, body = http_get(@ports.api, path)
      [parse_json(body), code]
    end

    def api_post(path, payload)
      code, body = http_post(@ports.api, path, payload && JSON.generate(payload))
      [parse_json(body), code]
    end

    def parse_json(body)
      JSON.parse(body)
    rescue JSON::ParserError
      {}
    end

    def http_get(port, path)
      Net::HTTP.start('127.0.0.1', port, open_timeout: 2, read_timeout: 10) do |http|
        response = http.get(path)
        [response.code.to_i, response.body.to_s]
      end
    end

    def http_post(port, path, body = nil)
      Net::HTTP.start('127.0.0.1', port, open_timeout: 2, read_timeout: 60) do |http|
        response = http.post(path, body || '', { 'Content-Type' => 'application/json' })
        [response.code.to_i, response.body.to_s]
      end
    end
  end
end
