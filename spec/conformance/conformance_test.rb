# frozen_string_literal: true

# Conformance suite for docs/orchestrator-spec.md.
#
# Run against any implementation:
#
#   ORCHESTRATOR_ADAPTER=spec/conformance/adapters/go   ruby spec/conformance/conformance_test.rb
#   ORCHESTRATOR_ADAPTER=spec/conformance/adapters/ruby ruby spec/conformance/conformance_test.rb
#
# Rules this file lives by, because they are what make it a spec suite rather
# than a second description of one implementation:
#
#   1. Only the three documented endpoints, plus the app's public port.
#   2. No knowledge of how an implementation stores anything. No slot directory
#      names, no symlinks, no log files, no process inspection.
#   3. Only the status fields the spec defines. An implementation may return
#      more; this suite must not require them.
#   4. Where the spec permits a choice, accept every permitted answer.
#
# Anything that cannot be expressed within those rules belongs in an
# implementation's own tests, not here.

$LOAD_PATH.unshift(__dir__)

require 'minitest/autorun'
require 'time'
require 'support/harness'

class ConformanceTest < Minitest::Test
  # Each test gets its own orchestrator, repository and ports.
  def setup
    @orch = nil
  end

  def teardown
    @orch&.stop
  end

  def start(**options)
    @orch = Conformance::Orchestrator.new(**options).start
  end

  def repo
    @orch.repo
  end

  # The spec defines exactly these status fields. Asserting on the set keeps an
  # implementation from passing by returning something differently shaped.
  REQUIRED_STATUS_FIELDS = %w[
    live_slot live_commit previous_slot previous_commit last_deploy_time healthy
  ].freeze

  # ---------------------------------------------------------------------------
  # The status contract
  # ---------------------------------------------------------------------------

  # covers: R26 R27
  def test_status_returns_the_documented_fields
    start
    status = @orch.status

    missing = REQUIRED_STATUS_FIELDS - status.keys
    assert_empty missing, "GET /status is missing documented fields: #{missing.join(', ')}"
  end

  # ---------------------------------------------------------------------------
  # Scenario 1: deploy, health check passes, traffic routes to the new slot
  # ---------------------------------------------------------------------------

  # covers: R5 R13 R14 R15
  def test_deploy_routes_traffic_to_the_new_version
    start
    response = @orch.deploy!(repo.healthy)

    assert_equal repo.healthy, response['commit'],
                 'a successful deploy must report the commit it made live'
    refute_nil response['slot'], 'a successful deploy must name the slot'

    # Ask the app, not the orchestrator: this is the only way to know traffic
    # actually reached the version that was deployed.
    assert @orch.wait_for_public_version('v1'),
           "the public port served #{@orch.public_version.inspect}, expected the deployed version"

    status = @orch.status
    assert_equal repo.healthy, status['live_commit']
    assert_equal true, status['healthy']
  end

  # covers: R15
  def test_deploying_a_second_version_switches_traffic
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')

    @orch.deploy!(repo.second)
    assert @orch.wait_for_public_version('v2'),
           'traffic still reaches the old version after a successful deploy'
    assert_equal repo.second, @orch.status['live_commit']
  end

  # ---------------------------------------------------------------------------
  # Scenario 2: deploy, health check fails, traffic stays on the old slot
  # ---------------------------------------------------------------------------

  # covers: R7 R16 R17
  def test_failed_health_check_leaves_the_previous_version_serving
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')
    before = @orch.status

    response, code = @orch.deploy(repo.unhealthy)

    refute response['success'], 'an app that never becomes healthy must not be promoted'
    refute_equal 200, code, 'a refused deploy must not report HTTP 200'

    assert_equal 'v1', @orch.public_version,
                 'the previously live version must still be serving traffic'
    assert_equal before['live_commit'], @orch.status['live_commit'],
                 'a failed deploy must not change which commit is live'
  end

  # covers: R16
  def test_an_app_that_boots_too_slowly_fails_its_health_check
    start(health_timeout_ms: 800)
    @orch.deploy!(repo.healthy)

    response, = @orch.deploy(repo.slow)

    refute response['success'],
           'an app that is not healthy within health_timeout_ms must not be promoted'
    assert_equal 'v1', @orch.public_version
  end

  # ---------------------------------------------------------------------------
  # Scenario 3: deploy, then rollback, traffic returns to the previous slot
  # ---------------------------------------------------------------------------

  # covers: R20
  def test_rollback_returns_traffic_to_the_previous_version
    start
    @orch.deploy!(repo.healthy)
    @orch.deploy!(repo.second)
    assert @orch.wait_for_public_version('v2')

    response = @orch.rollback!

    assert_equal repo.healthy, response['commit'],
                 'rollback must report the commit it restored'
    assert @orch.wait_for_public_version('v1'),
           'traffic did not return to the previous version'
    assert_equal repo.healthy, @orch.status['live_commit']
  end

  # covers: R20
  def test_deploy_after_rollback_moves_forward_again
    start
    @orch.deploy!(repo.healthy)
    @orch.deploy!(repo.second)
    @orch.rollback!
    assert @orch.wait_for_public_version('v1')

    @orch.deploy!(repo.third)
    assert @orch.wait_for_public_version('v3'),
           'a deploy after a rollback must still promote normally'
  end

  # ---------------------------------------------------------------------------
  # Scenario 4: deploy twice, only one previous slot is retained
  # ---------------------------------------------------------------------------

  # The spec says one previous slot is retained. It does not say how an
  # implementation stores slots, so this is asserted the only way it can be from
  # outside: after three deploys the reported previous commit is the second, and
  # a single rollback goes there and no further.
  # covers: R22
  def test_only_one_previous_version_is_retained
    start
    @orch.deploy!(repo.healthy)
    @orch.deploy!(repo.second)
    assert_equal repo.healthy, @orch.status['previous_commit']

    @orch.deploy!(repo.third)
    assert_equal repo.second, @orch.status['previous_commit'],
                 'only the immediately previous deploy may be retained'

    @orch.rollback!
    assert @orch.wait_for_public_version('v2')

    # There is no second step back: rolling back again may only return to what
    # was live a moment ago, never to the version before that.
    @orch.rollback
    refute_equal 'v1', @orch.public_version,
                 'rollback reached two deploys back; only one previous slot may be kept'
  end

  # ---------------------------------------------------------------------------
  # Scenario 5: rollback with no previous slot is an error
  # ---------------------------------------------------------------------------

  # covers: R20
  def test_rollback_without_a_previous_version_is_refused
    start
    # Whether the implementation deployed HEAD at startup or not, there has been
    # no second deploy, so there is nothing to roll back to.
    response, code = @orch.rollback

    refute response['success'], 'rollback with no previous slot must be refused'
    refute_equal 200, code, 'a refused rollback must not report HTTP 200'
  end

  # ---------------------------------------------------------------------------
  # Scenario 6: deploying while a deploy is in progress
  # ---------------------------------------------------------------------------

  # The spec permits either queueing or rejection, so this accepts both. What it
  # does not permit is two deploys proceeding at once, which would leave the
  # implementation's idea of what is live racing itself.
  # covers: R18
  def test_a_deploy_during_a_deploy_is_queued_or_rejected
    start(health_timeout_ms: 10_000)
    @orch.deploy!(repo.healthy)

    # The slow app takes five seconds to become healthy, so the first deploy is
    # still in flight while the second is sent.
    slow = Thread.new { @orch.deploy(repo.slow) }
    sleep 1.0

    second_response, second_code = @orch.deploy(repo.second)
    slow_response, = slow.value

    if second_response['success']
      # Queued: both deploys ran, one after the other, and the last one wins.
      assert @orch.wait_for_public_version('v2'),
             'the second deploy reported success but its version is not serving'
    else
      # Rejected: the first deploy is unaffected and the refusal is visible.
      refute_equal 200, second_code, 'a rejected deploy must not report HTTP 200'
      assert slow_response['success'],
             'rejecting the second deploy must not disturb the first'
      assert @orch.wait_for_public_version('slow')
    end
  end

  # ---------------------------------------------------------------------------
  # Scenario 7: the live process crashes after promotion
  # ---------------------------------------------------------------------------

  # covers: R29
  def test_a_crash_after_promotion_is_reflected_in_status
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')
    assert_equal true, @orch.status['healthy']

    @orch.app_control('crash')

    deadline = Time.now + 15
    until @orch.status['healthy'] == false
      flunk 'status still reports healthy after the live process crashed' if Time.now > deadline
      sleep 0.1
    end
  end

  # The public port belongs to the orchestrator, not to the app process, so a
  # crashed app must present as an unavailable service rather than as nothing at
  # all. A refused connection is indistinguishable from the whole machine being
  # gone, and releasing the port lets something else claim it.
  # covers: R31
  def test_the_public_port_answers_503_after_a_crash
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')

    @orch.app_control('crash')

    assert @orch.wait_for_public_code(503),
           'the public port must answer 503 once nothing is live, not refuse connections'
  end

  # ---------------------------------------------------------------------------
  # Scenario 8: drain timeout exceeded
  # ---------------------------------------------------------------------------

  # An app that ignores SIGTERM must not be able to hold a deploy open forever.
  # The new version has to end up serving, which means the old process was
  # eventually killed rather than waited on.
  # covers: R23 R24
  def test_an_app_that_ignores_sigterm_is_force_killed
    start(drain_timeout_ms: 1_000)
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')

    @orch.app_control('hang')

    started = Time.now
    @orch.deploy!(repo.second)
    assert @orch.wait_for_public_version('v2'),
           'a deploy did not complete while the old process was ignoring SIGTERM'

    # Bounded by the drain timeout, not by the old process's patience.
    assert_operator Time.now - started, :<, 25,
                    'the deploy waited far longer than drain_timeout_ms on an unresponsive process'
  end

  # ---------------------------------------------------------------------------
  # The app contract
  # ---------------------------------------------------------------------------

  # The app is told its ports through the environment. Without this an app cannot
  # be written against an orchestrator at all: it has no other way to learn which
  # port to bind, since the orchestrator assigns them per deploy.
  # covers: R6
  def test_the_app_is_given_its_ports_through_the_environment
    start
    @orch.deploy!(repo.healthy)

    # testapp exits 64 if PORT or INTERNAL_PORT is missing, so it could not have
    # become healthy without them.
    assert_equal 'v1', @orch.public_version,
                 'the app could not have served traffic without PORT and INTERNAL_PORT'
  end

  # Public traffic must reach the app's public port, never its internal one.
  #
  # Asserted by effect rather than by status code, because the status code cannot
  # distinguish the two: this app answers every public path with 200. So send the
  # request that would be destructive if it were routed internally, and require
  # that nothing happened.
  # covers: R8
  def test_public_traffic_does_not_reach_the_internal_port
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')

    @orch.send(:http_post, @orch.ports.app, '/control/crash')
    sleep 0.5

    assert_equal 'v1', @orch.public_version,
                 'a crash request through the public port killed the app: public traffic ' \
                 'is reaching the internal port, which would expose the control endpoints'
  end

  # ---------------------------------------------------------------------------
  # Deploy input handling
  # ---------------------------------------------------------------------------

  # covers: R12
  def test_a_deploy_without_a_commit_is_refused
    start
    response, code = @orch.send(:api_post, '/deploy', {})

    refute response['success'], 'a deploy with no commit must be refused'
    refute_equal 200, code
  end

  # covers: R11
  def test_a_deploy_of_an_unknown_commit_is_refused
    start
    @orch.deploy!(repo.healthy)

    response, code = @orch.deploy('0' * 40)

    refute response['success'], 'a deploy of a commit that does not exist must be refused'
    refute_equal 200, code
    assert_equal 'v1', @orch.public_version, 'the live version must be undisturbed'
  end

  # ---------------------------------------------------------------------------
  # Startup and lifecycle
  # ---------------------------------------------------------------------------

  # The harness relies on this for every other test: it waits for GET / before
  # doing anything. R2 is bound up with it — if an implementation answered here
  # while still deploying HEAD, the first deploy in every test would race the
  # startup one, and the suite would be permanently flaky rather than wrong once.
  # covers: R1 R2
  def test_the_api_answers_get_slash_when_ready
    start
    body, code = @orch.send(:api_get, '/')

    assert_equal 200, code, 'GET / must answer 200 once the orchestrator is ready'
    refute_nil body, 'GET / must return a body the client can at least parse'

    # Ready means ready: an immediate deploy must not be refused as concurrent.
    response, deploy_code = @orch.deploy(repo.healthy)
    assert response['success'],
           "a deploy immediately after readiness was refused (HTTP #{deploy_code}): " \
           'startup work must finish before GET / answers'
  end

  # The public port is the orchestrator's, so it answers before any deploy has
  # succeeded — with 503, because there is nothing behind it yet. This is the
  # case that matters most in practice: an operator whose first deploy fails
  # needs the port to tell them so.
  # covers: R3 R31
  def test_the_public_port_is_bound_before_any_successful_deploy
    start(health_timeout_ms: 800)

    # Nothing healthy has ever been deployed in this orchestrator except possibly
    # HEAD at startup, so first make sure nothing is live.
    @orch.deploy(repo.unhealthy)

    if @orch.status['live_commit'].to_s.empty?
      assert @orch.wait_for_public_code(503),
             'with nothing live, the public port must answer 503 rather than refuse'
    else
      # The implementation deployed HEAD at startup, which is permitted. Crash it
      # and check the port stays bound.
      @orch.app_control('crash')
      assert @orch.wait_for_public_code(503),
             'once nothing is live, the public port must answer 503 rather than refuse'
    end
  end

  # covers: R4
  def test_sigterm_stops_the_orchestrator_and_the_app
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')
    app_port = @orch.ports.app

    # stop raises if the orchestrator does not exit on SIGTERM.
    @orch.stop
    @orch = nil

    # And the app it started must not outlive it, or the next run inherits a
    # process holding the port.
    deadline = Time.now + 10
    loop do
      begin
        Net::HTTP.start('127.0.0.1', app_port, open_timeout: 0.5, read_timeout: 0.5) { |h| h.get('/') }
      rescue StandardError
        break # port is gone, as it should be
      end
      flunk 'the app is still serving after the orchestrator exited' if Time.now > deadline
      sleep 0.1
    end
  end

  # ---------------------------------------------------------------------------
  # The app contract
  # ---------------------------------------------------------------------------

  # setup_command runs in the slot, before the app. Proved by having it rewrite
  # the VERSION file that the app then serves: if the app reports the value setup
  # wrote, setup ran first and ran in the right directory.
  # covers: R5 R10
  def test_setup_command_runs_in_the_slot_before_the_app
    start(contract: { 'setup_command' => "printf 'from-setup\n' > VERSION" })
    @orch.deploy!(repo.healthy)

    assert @orch.wait_for_public_version('from-setup'),
           'the app did not see the file setup_command wrote, so setup did not run ' \
           'in the slot before the app started'
  end

  # The stable internal port must address the live app's internal port, or the
  # contract field names nothing. Every control call in this suite depends on it.
  # covers: R9
  def test_the_internal_port_reaches_the_live_app
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')

    code, body = @orch.send(:http_get, @orch.ports.internal, '/healthz')
    assert_equal 200, code,
                 'the health endpoint must be reachable on the contract internal_port'
    assert_includes body, 'ok'
  end

  # ---------------------------------------------------------------------------
  # Zero downtime
  # ---------------------------------------------------------------------------

  # Traffic switches before the old process is asked to stop, so a request in
  # flight during a deploy is never sent to something shutting down. Asserted by
  # hammering the public port throughout a deploy: every response must be a 200
  # from one version or the other, never a failure and never a 5xx.
  # covers: R25
  def test_no_request_fails_during_a_deploy
    start
    @orch.deploy!(repo.healthy)
    assert @orch.wait_for_public_version('v1')

    stop = false
    failures = []
    hammer = Thread.new do
      until stop
        begin
          code, body = @orch.send(:http_get, @orch.ports.app, '/')
          failures << "HTTP #{code}: #{body}" unless code == 200
        rescue StandardError => e
          failures << e.class.name
        end
        sleep 0.02
      end
    end

    @orch.deploy!(repo.second)
    assert @orch.wait_for_public_version('v2')
    stop = true
    hammer.join

    assert_empty failures.uniq.first(5),
                 "requests failed while a deploy was in progress: #{failures.uniq.first(5).join(', ')}"
  end

  # ---------------------------------------------------------------------------
  # Status details
  # ---------------------------------------------------------------------------

  # covers: R27
  def test_status_fields_have_the_documented_types
    start
    status = @orch.status

    %w[live_slot live_commit previous_slot previous_commit last_deploy_time].each do |field|
      assert_kind_of String, status[field],
                     "#{field} must be a string — empty, not null or absent, when there is " \
                     'nothing to report'
    end
    assert_includes [true, false], status['healthy'], 'healthy must be a boolean'
  end

  # covers: R28
  def test_last_deploy_time_is_rfc3339
    start
    before = @orch.status['last_deploy_time']
    assert_kind_of String, before

    @orch.deploy!(repo.healthy)
    after = @orch.status['last_deploy_time']

    refute_empty after, 'last_deploy_time must be set once a deploy has happened'
    parsed = begin
      Time.iso8601(after)
    rescue ArgumentError
      nil
    end
    refute_nil parsed, "last_deploy_time #{after.inspect} is not RFC 3339"
  end
end

