# Working on teploy-dash

Instructions for coding agents. `CLAUDE.md` carries the detail on how this
codebase is built and tested; this file carries the routing rule for defects
that are not ours.

## Which layer owns a defect

Dash is a face over `teploy-cli`: a great deal of what it appears to do, the CLI
actually does. So the first question about any fault is which side owns it, and
the answer is often sideways rather than up.

**Fix a defect in the repository that owns it.**

- A view, a form, a route, an auth check, a thing rendered wrongly — ours.
- A deploy that behaves differently from what `teploy` does on the command
  line — **teploy-cli's**, and patching around it here makes the two disagree
  in a way that is very hard to see later.
- A store that answers a correct query wrongly, or a framework that loses
  data — upstream, in Neutron or Nucleus.

Where you can only work around it from this side, the workaround ships **with a
logged report**, not instead of one.

**Never cut a Neutron or Nucleus release from a Teploy session.** An upstream fix
is a standalone change in the upstream repo plus a written handover.

In Tyler's own checkout, Neutron and Nucleus findings go in
`Teploy/_internal/UPSTREAM_BUGS.md` and cross-Teploy findings in the umbrella
queue (both private, not part of this repository). Working from a clone without
them, open the report in the owning repository and say in the commit message
that you did.
