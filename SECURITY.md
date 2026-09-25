# Security policy

## Reporting a vulnerability

Send it to `ask@stokaro.com`, not to the issue tracker. An issue is public from
the moment it is filed, and a report there tells everybody who reads it how to
reproduce the problem before there is a release that fixes it.

Write `security` in the subject so it is not read as a commercial enquiry.

Include what somebody needs to reproduce it:

- the chart version, and the manager, executor, and runner image digests;
- the Kubernetes version and distribution;
- the database engine and server version, where the report involves one;
- the resources involved, with credentials and connection strings removed;
- what the operator did, what you expected instead, and what an attacker gains.

You will get a reply saying whether the report is understood and what happens
next. A fix ships in a release that says what it changed, on
[Release notes](https://operator.ptah.run/edge/support/release-notes/), and the
advisory is published with it.

## What is in scope

The operator's job is to hold four authorities apart -- who writes desired
state, who approves a plan, what the controller may write, and what credentials
a Job receives -- and to bind evidence to the exact artifact it was produced
from. A way to cross one of those boundaries is what this policy is for:

- running SQL a plan does not contain, or applying a plan nobody approved;
- reaching a credential from a resource or a Job that should not have it;
- making the operator write an object its admission policies refuse, or writing
  one on its behalf;
- passing evidence, a plan, an approval, or an image off as one it is not;
- making the operator report an Apply as settled when it is not, or lose the
  record that it is unresolved.

[Security model](https://operator.ptah.run/edge/use/security/) says how those
boundaries are drawn and which component holds each one.

## What is not

- A cluster where the reporter already holds the privileges in question. An
  account that may edit a `PtahSchema` may select `spec.policy.apply: Always`,
  and that is the resource's own field rather than a bypass; an account that
  may write the controller's objects may do what the controller does.
- The database's own privileges. The operator runs what the credentials it is
  given allow, and granting a cluster-wide administrative account is a
  deployment decision the
  [security model](https://operator.ptah.run/edge/use/security/) asks you not
  to make.
- The deployment responsibilities that page lists as yours: NetworkPolicies,
  image pinning, protecting the shared target-lock namespace, one release per
  cluster.
- Ptah itself. Schema rendering, migration files, and dialect support belong in
  [stokaro/ptah](https://github.com/stokaro/ptah/security).

## Supported versions

There is one supported version: the newest published release. Before `v1` there
are no deprecations and no backports, and
[API compatibility](https://operator.ptah.run/edge/support/api-compatibility/)
says what that means for objects you have already stored. Pin the chart version
you deploy and read what a release changed before you move to it.
