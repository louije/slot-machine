# frozen_string_literal: true

require 'fileutils'
require 'json'
require 'net/http'
require 'open3'
require 'time'

require_relative 'http'
require_relative 'proxy'

module SlotMachine
  # Slot is one checked-out version of the app and the process running it.
  class Slot
    attr_reader :name, :commit, :dir, :pid, :app_port, :internal_port
    attr_accessor :alive

    def initialize(name:, commit:, dir:, pid:, app_port:, internal_port:)
      @name = name
      @commit = commit
      @dir = dir
      @pid = pid
      @app_port = app_port
      @internal_port = internal_port
      @alive = true
    end
  end

  # Orchestrator implements docs/orchestrator-spec.md.
  #
  # Two deliberate differences from slot-machine, to keep the spec honest about
  # what it does and does not require:
  #
  #   - Slots are produced with `git archive`, not `git worktree`. A slot here is
  #     a plain directory with no .git in it at all. The spec says an
  #     implementation checks a commit out somewhere; it does not say how, and
  #     this proves it.
  #   - Slots are named by generation, not by commit hash, so redeploying the
  #     same commit needs no special handling. The spec does not define slot
  #     names, and nothing outside this class depends on them.
  class Orchestrator
    def initialize(contract:, repo_dir:, data_dir:)
      @contract = contract
      @repo_dir = repo_dir
      @data_dir = data_dir
      @slots_dir = File.join(data_dir, 'slots')
      FileUtils.mkdir_p(@slots_dir)

      @mutex = Mutex.new
      @deploying = false
      @live = nil
      @previous = nil
      @last_deploy = nil
      @generation = 0

      @app_proxy = Proxy.new(@contract.port, name: 'public')
      # The stable internal port is proxied to the live slot's internal port.
      # Without this there is no fixed address for the health or schema endpoints
      # of whatever happens to be live, which is the only reason the contract
      # names a port at all.
      @internal_proxy =
        if @contract.internal_port != @contract.port
          Proxy.new(@contract.internal_port, name: 'internal')
        end

      reap_children
    end

    # start_proxies binds the public ports before anything is deployed.
    def start_proxies
      @app_proxy.start
      @internal_proxy&.start
    end

    def stop
      drain_all
      @app_proxy.stop
      @internal_proxy&.stop
    end

    # --- the spec's three endpoints -----------------------------------------

    def deploy(commit)
      claim = @mutex.synchronize do
        if @deploying
          nil
        else
          @deploying = true
          true
        end
      end
      # The spec permits queueing or rejection. Rejecting is the simpler of the
      # two to reason about: a caller learns immediately rather than blocking on
      # someone else's deploy.
      return [{ success: false, error: 'a deploy is already in progress' }, 409] if claim.nil?

      begin
        do_deploy(commit)
      ensure
        @mutex.synchronize { @deploying = false }
      end
    end

    def rollback
      claim = @mutex.synchronize do
        if @deploying
          nil
        else
          @deploying = true
          true
        end
      end
      return [{ success: false, error: 'a deploy is already in progress' }, 409] if claim.nil?

      begin
        do_rollback
      ensure
        @mutex.synchronize { @deploying = false }
      end
    end

    def status
      @mutex.synchronize do
        {
          live_slot: @live&.name || '',
          live_commit: @live&.commit || '',
          previous_slot: @previous&.name || '',
          previous_commit: @previous&.commit || '',
          last_deploy_time: @last_deploy ? @last_deploy.utc.iso8601 : '',
          healthy: !@live.nil? && @live.alive
        }
      end
    end

    # --- deploy --------------------------------------------------------------

    private

    def do_deploy(commit)
      resolved = resolve_commit(commit)
      return [{ success: false, error: "unknown commit #{commit.inspect}" }, 400] if resolved.nil?

      previous_commit = @mutex.synchronize { @live&.commit || '' }

      slot_dir, slot_name = checkout(resolved)
      run_setup(slot_dir) if @contract.setup_command

      slot = boot(slot_dir, slot_name, resolved)
      return [{ success: false, error: 'the app could not be started' }, 500] if slot.nil?

      unless healthy?(slot)
        kill(slot)
        FileUtils.rm_rf(slot_dir)
        return [{
          success: false,
          error: "the new process did not pass #{@contract.health_endpoint} " \
                 "within #{@contract.health_timeout_ms}ms; the live slot is untouched"
        }, 422]
      end

      promote(slot)

      [{ success: true, slot: slot.name, commit: resolved, previous_commit: previous_commit }, 200]
    end

    def do_rollback
      target = @mutex.synchronize { @previous }
      return [{ success: false, error: 'no previous slot to roll back to' }, 400] if target.nil?

      # The previous slot's directory is still on disk, but its process is not
      # running: boot it again on fresh ports and hold it to the same health
      # contract as any other deploy. A rollback that skipped the health check
      # could take the app from broken to absent.
      slot = boot(target.dir, target.name, target.commit)
      return [{ success: false, error: 'the previous version could not be started' }, 500] if slot.nil?

      unless healthy?(slot)
        kill(slot)
        return [{ success: false, error: 'the previous version did not pass its health check' }, 422]
      end

      old_live = @mutex.synchronize { @live }
      switch_traffic(slot)
      @mutex.synchronize do
        @live = slot
        @previous = old_live
        @last_deploy = Time.now
      end
      drain(old_live) if old_live

      [{ success: true, slot: slot.name, commit: slot.commit }, 200]
    end

    # promote swaps traffic to slot and retires what was live.
    def promote(slot)
      old_live, old_previous = @mutex.synchronize { [@live, @previous] }

      switch_traffic(slot)

      # Update state before draining, so a crash callback firing during the drain
      # cannot clear the target that was just set.
      @mutex.synchronize do
        @live = slot
        @previous = old_live
        @last_deploy = Time.now
      end

      drain(old_live) if old_live

      # Exactly one previous slot is retained, so the one before it goes.
      return if old_previous.nil?

      drain(old_previous)
      FileUtils.rm_rf(old_previous.dir)
    end

    def switch_traffic(slot)
      @app_proxy.target = slot.app_port
      @internal_proxy&.target = slot.internal_port
    end

    # --- git -----------------------------------------------------------------

    def resolve_commit(commit)
      return nil if commit.nil? || commit.to_s.strip.empty?

      out, status = Open3.capture2e('git', '-C', @repo_dir, 'rev-parse', '--verify', '--quiet',
                                    "#{commit}^{commit}")
      status.success? ? out.strip : nil
    end

    # checkout materialises a commit as a plain directory tree.
    #
    # `git archive` rather than a worktree: the result has no .git, which keeps
    # slots inert and makes it obvious that the spec does not require any
    # particular git mechanism.
    def checkout(commit)
      generation = @mutex.synchronize { @generation += 1 }
      name = format('slot-%03d-%s', generation, commit[0, 8])
      dir = File.join(@slots_dir, name)
      FileUtils.mkdir_p(dir)

      archive, status = Open3.capture2('git', '-C', @repo_dir, 'archive', '--format=tar', commit,
                                       binmode: true)
      raise "git archive #{commit} failed" unless status.success?

      IO.popen(['tar', '-x', '-C', dir], 'wb') { |io| io.write(archive) }
      [dir, name]
    end

    def run_setup(dir)
      Open3.capture2e({ 'PATH' => ENV['PATH'] }, '/bin/sh', '-c', @contract.setup_command, chdir: dir)
    end

    # --- processes -----------------------------------------------------------

    def boot(dir, name, commit)
      app_port = HTTP.free_port
      internal_port = HTTP.free_port

      env = ENV.to_h.merge(@contract.env_pairs(@repo_dir)).merge(
        # How the app learns which ports to bind. It cannot be told any other
        # way: the orchestrator picks them per boot so that the old and new
        # processes can run at the same time.
        'PORT' => app_port.to_s,
        'INTERNAL_PORT' => internal_port.to_s
      )

      log_path = File.join(@data_dir, "#{name}.log")
      log = File.open(log_path, 'a')

      pid = Process.spawn(env, '/bin/sh', '-c', @contract.start_command,
                          chdir: dir, out: log, err: log, pgroup: true)
      log.close

      slot = Slot.new(name: name, commit: commit, dir: dir, pid: pid,
                      app_port: app_port, internal_port: internal_port)
      watch(slot)
      slot
    rescue StandardError => e
      warn "boot: #{e.class}: #{e.message}"
      nil
    end

    # watch reaps the process and notices a crash.
    def watch(slot)
      Thread.new do
        begin
          Process.waitpid(slot.pid)
        rescue Errno::ECHILD
          nil
        end
        slot.alive = false
        @mutex.synchronize do
          next unless @live.equal?(slot)

          # Nothing is live now. Clear the target but keep listening, so the
          # public port reports an unavailable service rather than vanishing.
          @app_proxy.target = nil
          @internal_proxy&.target = nil
        end
      end
    end

    def healthy?(slot)
      deadline = Time.now + (@contract.health_timeout_ms / 1000.0)
      uri = URI("http://127.0.0.1:#{slot.internal_port}#{@contract.health_endpoint}")

      loop do
        return false unless slot.alive
        return false if Time.now > deadline

        begin
          response = Net::HTTP.start(uri.host, uri.port, open_timeout: 0.5, read_timeout: 0.5) do |http|
            http.get(uri.path)
          end
          return true if response.code.to_i == 200
        rescue StandardError
          # Not up yet.
        end
        sleep 0.1
      end
    end

    # drain asks a process to stop, then insists.
    #
    # An app that ignores SIGTERM must not be able to hold a deploy open for
    # ever, so the wait is bounded by drain_timeout_ms and ends in SIGKILL.
    def drain(slot)
      return if slot.nil? || slot.pid.nil?

      signal(slot, 'TERM')
      deadline = Time.now + (@contract.drain_timeout_ms / 1000.0)
      while slot.alive
        return if Time.now > deadline && !insist(slot)

        sleep 0.05
      end
    end

    def insist(slot)
      signal(slot, 'KILL')
      50.times do
        return false unless slot.alive

        sleep 0.05
      end
      false
    end

    def kill(slot)
      signal(slot, 'KILL')
      20.times { break unless slot.alive; sleep 0.05 }
    end

    def signal(slot, name)
      # The whole group: start_command runs under a shell, so the app is a
      # grandchild and signalling only the shell would orphan it.
      Process.kill("-#{name}", Process.getpgid(slot.pid))
    rescue Errno::ESRCH, Errno::EPERM
      nil
    end

    def drain_all
      [@live, @previous].compact.each { |slot| drain(slot) }
    end

    def reap_children
      # A previous run may have left processes behind; nothing here inherits
      # them, so there is nothing to reap in this implementation. Recording the
      # absence deliberately: the spec does not require restart recovery.
      nil
    end
  end
end
