---
name: awsmcproxy
description: Use when reaching AWS through an awsmcproxy MCP server - recognisable by a `list_profiles` tool and by every other tool taking a required `profile` argument. Covers choosing the profile, why the `aws` CLI is not a substitute, and how an expired SSO session or a narrowed role fails.
---

# awsmcproxy

[awsmcproxy](https://github.com/winebarrel/awsmcproxy) fronts the AWS MCP
Server. It mirrors that server's tools -- typically `call_aws` and
`suggest_aws_commands` -- and adds a required `profile` argument to each one.
The call is signed with that profile's credentials, so `profile` decides which
AWS account and identity the operation runs against.

## Choose the profile before the first call

Profile names are deliberately absent from the tool schemas: the proxy rereads
`~/.aws/config` and `~/.aws/credentials` on every `list_profiles` call, so a
profile added while the proxy is running is usable immediately.

1. Call `list_profiles` before the first AWS call of the session.
2. Match the names against what the user asked for.
3. If more than one name plausibly fits -- and especially if one of them looks
   like production -- ask which. Do not guess, and do not default to `default`
   because it is first. A wrong guess runs the operation against the wrong
   account.

Reuse the chosen profile for follow-up calls in the same task rather than
relisting, but call `list_profiles` again if the user names a profile you have
not seen.

## The `aws` CLI is not a fallback

If a tool call fails, do not retry the same operation through `Bash(aws ...)`.
A CLI call carries no `profile` argument, is not signed by the proxy, and drops
whatever narrowing the proxy was started with. Many setups deny `Bash(aws *)`
for exactly this reason; where it is not denied it still bypasses the intended
path. Report the failure instead.

## Failures

**Expired or missing SSO session** -- the call fails when the credentials are
first retrieved, not at startup. Ask the user to run `aws sso login --profile
<name>` themselves; it is interactive and cannot be completed on their behalf.

**`AccessDenied` on a write** -- the proxy may have been started with
`--sso-role`, which replaces `sso_role_name` for every profile and is how an
operator restricts an agent to, say, `ReadOnlyAccess`. Treat it as a decision,
not an obstacle: do not look for another profile, another role, or another path
to the same effect. Say what was denied and stop.

**A role not assigned to you in that account** -- same shape as above, and also
surfaces on first use rather than at startup.

## Region

Each profile's own region is sent with the call and is the default for the
operation. There is no per-call region argument. To work in another region,
either use a profile configured for it or pass the region through the AWS
operation itself, the way the underlying tool expects.
