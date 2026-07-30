# frozen_string_literal: true

# Checks that the spec and its suite have not drifted apart.
#
# Two failure modes this catches, both of which happened to this project before
# there was anything to catch them:
#
#   - A requirement is written into the spec and never tested, so the document
#     describes an aspiration rather than a checked property.
#   - A test claims to cover a requirement number that no longer exists, usually
#     after the spec was renumbered.
#
# Needs no orchestrator, so it runs in a second.

require 'minitest/autorun'

class CoverageTest < Minitest::Test
  SPEC = File.expand_path('../../docs/orchestrator-spec.md', __dir__)
  SUITE = File.expand_path('conformance_test.rb', __dir__)

  # Requirements deliberately not covered by the suite, each with the reason.
  #
  # An entry here is a claim that the requirement cannot be checked from outside,
  # not that testing it would be inconvenient. Keep it short and keep it argued.
  EXEMPT = {
    'R19' => 'a SHOULD, not a MUST: an implementation that omits the error field ' \
             'still conforms, so the suite cannot require it',
    'R21' => 'the rollback target is health-checked, but making it fail requires a ' \
             'commit whose app is unhealthy only on its second boot, and the only ' \
             'way to arrange that is to write into the previous slot — whose ' \
             'location the spec deliberately does not define',
    'R30' => 'a permission granted to implementations, not an obligation: there is ' \
             'nothing for a suite to verify'
  }.freeze

  def spec_requirements
    # Read as UTF-8 explicitly: the default external encoding depends on the
    # environment's locale, and the spec contains typography that is not ASCII.
    text = File.read(SPEC, encoding: 'UTF-8')
    # Requirements are declared in bold at the start of their paragraph.
    declared = text.scan(/^\*\*(R\d+)\.\*\*/).flatten
    assert_operator declared.size, :>, 20, 'failed to parse requirements out of the spec'
    declared
  end

  def covered_requirements
    File.read(SUITE, encoding: 'UTF-8').scan(/^\s*#\s*covers:\s*(.+)$/).flatten
        .flat_map(&:split)
        .map { |id| id.delete(',') }
        .uniq
  end

  def test_every_requirement_is_numbered_uniquely
    declared = spec_requirements
    duplicates = declared.tally.select { |_, n| n > 1 }.keys
    assert_empty duplicates, "the spec declares these requirements more than once: #{duplicates.join(', ')}"
  end

  def test_requirement_numbers_have_no_gaps
    numbers = spec_requirements.map { |id| Integer(id[1..]) }.sort
    expected = (1..numbers.max).to_a
    assert_equal expected, numbers,
                 'requirement numbers must be contiguous, or a reader cannot tell a ' \
                 'renumbering from a deletion'
  end

  def test_every_requirement_is_tested_or_explicitly_exempt
    declared = spec_requirements
    covered = covered_requirements

    untested = declared - covered - EXEMPT.keys
    assert_empty untested,
                 "these requirements have no conformance test and no exemption: " \
                 "#{untested.join(', ')}.\nAdd a test, or add an entry to EXEMPT " \
                 'in this file explaining why it cannot be checked from outside.'
  end

  def test_the_suite_does_not_cite_requirements_that_do_not_exist
    declared = spec_requirements
    stale = covered_requirements - declared

    assert_empty stale,
                 "the suite claims to cover requirements that the spec does not " \
                 "declare: #{stale.join(', ')}. The spec was probably renumbered."
  end

  def test_exemptions_refer_to_real_requirements
    declared = spec_requirements
    stale = EXEMPT.keys - declared

    assert_empty stale, "exemptions for requirements that no longer exist: #{stale.join(', ')}"
  end

  def test_exemptions_are_justified
    EXEMPT.each do |id, reason|
      refute_nil reason, "#{id} is exempt with no reason"
      assert_operator reason.length, :>, 40,
                      "#{id}'s exemption reason is too short to be an argument"
    end
  end
end
