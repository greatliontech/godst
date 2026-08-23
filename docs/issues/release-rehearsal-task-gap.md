# The extraction-validation workflow glue is unrehearsable until a tag carries its tasks

A release rehearsal (workflow_dispatch on an existing tag) checks out the
TAG's tree — composite action, Taskfile, scripts — so only `release.yml`
runs at the dispatched ref (releases.md, "Continuous integration"). No
existing `go*-dst.*` tag carries `dist:extract`/`verify:dist`/`smoke:dist`,
so a rehearsal of the extraction-validation steps fails with "task does not
exist" and cannot exercise the new workflow glue (the asset-name
interpolation, `$RUNNER_TEMP` extraction, `GITHUB_ENV` hand-off).

Until the first release tag cut from a tree carrying the tasks, that glue
first executes on a real tag; a failure there burns a release counter — a
cost the release contract already prices ("a tag whose workflow fails is
not a release: fix forward, cut the next counter"). The task bodies
themselves are covered by local runs against a real distpack tarball; only
the yaml plumbing is exposed.

Lands: when the first `go*-dst.*` tag whose tree carries `verify:dist` and
`smoke:dist` exists — rehearsals dispatched on it then cover the glue, and
this gap closes for every later workflow change.
