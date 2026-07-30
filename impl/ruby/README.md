# impl/ruby — a conformance fixture

**Do not deploy this.** It is not a smaller slot-machine and it is not a
Ruby-flavoured alternative. It exists for one purpose: to stop
`docs/orchestrator-spec.md` from being a description of the Go implementation.

## Why it has to exist

A conformance suite that has only ever run against one implementation encodes
that implementation's assumptions invisibly. Writing this one surfaced eleven
requirements the spec had never stated — including how an app learns which port
to bind, whether the API has a readiness endpoint, and whether a crashed app
should refuse connections or answer 503. None of them would have been found by
adding more tests to the Go suite, because the Go suite and the Go implementation
agreed with each other about all of them.

Delete this, and the spec reverts to being a description of implementation 1
within about one refactor. `spec/conformance/coverage_test.rb` would not notice:
it checks that every requirement has a test, not that two implementations pass.

## Why Go remains the implementation you run

The deciding reason is not maturity, it is runtime coupling. An orchestrator sits
beside apps that might be Node, Python or Ruby, and it has to outlive their
deploys. A Ruby orchestrator on a box whose app is also Ruby couples the two: the
version the app needs and the version the orchestrator needs become the same
decision, and an app that upgrades its runtime can take its supervisor down with
it. The Go implementation is a static binary with no such coupling, which is the
whole argument in `docs/orchestrator-spec.md` §"Implementation Notes".

Beyond that, this implementation is deliberately incomplete. It has the spec's
core and nothing else: no agent, no chat, no deploy gate, no machine branch, no
schema compatibility checks, and no restart recovery. Its HTTP server and proxy
are hand-rolled to the minimum the suite exercises — no keep-alive, no
websockets, no request limits. Fine for a fixture. Not fine in front of traffic.

## What it is useful for besides conformance

It is the readable reference. It is about 800 lines, it implements exactly the 31
requirements, and it is the fastest way to understand what an orchestrator has to
do without reading a codebase that also contains an agent supervisor, a deploy
gate and a chat UI. `lib/orchestrator.rb` is the file worth reading.

## Deliberate differences from the Go implementation

These are not accidents of a second attempt. Each one is a place where the spec
allows a choice, and making the opposite choice is what proves the spec allows
it:

| | Go | Ruby |
|---|---|---|
| Slot contents | `git worktree` — slot contains a `.git` file | `git archive` — slot has no git at all |
| Slot naming | by commit hash | by generation number |
| Concurrent deploy | rejected | rejected (spec permits queueing too) |
| Restart recovery | recovers the live slot from symlinks | none; out of scope per the spec |

## Running it

Through the conformance suite, which is the only supported way:

```sh
ORCHESTRATOR_ADAPTER=spec/conformance/adapters/ruby ruby spec/conformance/conformance_test.rb
```

Directly, if you want to poke at it:

```sh
ruby impl/ruby/slot_machine.rb --config <contract.json> --repo <dir> --data <dir> --port 9100
```
