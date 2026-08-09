# awsmcproxy

[![CI](https://github.com/winebarrel/awsmcproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/winebarrel/awsmcproxy/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/winebarrel/awsmcproxy/branch/main/graph/badge.svg)](https://codecov.io/gh/winebarrel/awsmcproxy)
[![AI Generated](https://img.shields.io/badge/AI%20Generated-Claude-orange?logo=anthropic)](https://claude.ai/claude-code)

A multi-profile proxy for the [AWS MCP Server](https://docs.aws.amazon.com/agent-toolkit/latest/userguide/getting-started-aws-mcp-server.html).

The AWS MCP Server authenticates each request with SigV4, so one connection
carries one AWS identity. `awsmcproxy` opens a connection per AWS profile,
mirrors the server's tools, and adds a `profile` argument to each tool. When a
tool is called, the proxy signs the request with that profile's credentials.

```
Claude Code ──stdio──▶ awsmcproxy ──┬─ profile=dev  ─SigV4(dev)──▶ https://aws-mcp.us-east-1.api.aws/mcp
                                    └─ profile=prod ─SigV4(prod)─▶ https://aws-mcp.us-east-1.api.aws/mcp
```

There is nothing to configure. Profiles come from `~/.aws/config` and
`~/.aws/credentials`, and a `list_profiles` tool tells the agent which names it
can use.

It speaks the streamable HTTP transport and signs requests itself, so
[mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws) and `uvx` are not
needed.

## Requirements

AWS credentials reachable through the standard credential chain: `aws login`,
`aws sso login`, assumed roles, `credential_process` and static keys all work.

```
aws login          # or: aws sso login --profile my-dev
aws sts get-caller-identity
```

## Install

```
go install github.com/winebarrel/awsmcproxy/cmd/awsmcproxy@latest
```

## Usage

```
Usage: awsmcproxy [flags]

Flags:
  -h, --help               Show help.
  -e, --endpoint="https://aws-mcp.us-east-1.api.aws/mcp"
                           AWS MCP Server endpoint ($AWSMCPROXY_ENDPOINT).
      --sso-role=STRING    Override sso_role_name for every profile, e.g.
                           ReadOnlyAccess ($AWSMCPROXY_SSO_ROLE).
      --version
```

The endpoint is a full URL, not a region, so it also reaches
`https://aws-mcp.eu-central-1.api.aws/mcp` or a Bedrock AgentCore gateway. The
SigV4 service and region are inferred from its hostname.

### Reaching AWS through a narrower role

`--sso-role` replaces `sso_role_name` for every profile, keeping each profile's
own `sso_account_id` and SSO session:

```
awsmcproxy --sso-role ReadOnlyAccess
```

This is the way to stop an agent writing to AWS. Hiding write-capable tools
would not be a boundary; a role that lacks the permissions is one.

The SSO access token is per session, not per role, so the role is swapped
without logging in again. A role that is not assigned to you in that account
fails when the credentials are first used, not at startup.

Only IAM Identity Center can do this: its `GetRoleCredentials` takes an account
and a role *name*, whereas `AssumeRole` needs the role's full ARN, which differs
per account. A profile that does not use IAM Identity Center is left alone and
keeps its own credentials.

### Claude Code

Register it as an MCP server:

```json
{
  "mcpServers": {
    "awsmcproxy": {
      "command": "awsmcproxy"
    }
  }
}
```

Registering the proxy does not stop the agent reaching AWS another way. The
`aws` CLI is right there, and a call through it carries no `profile` argument,
is not signed by the proxy, and keeps none of the narrowing `--sso-role` was
set up to do. Deny it:

```json
{
  "permissions": {
    "deny": ["Bash(aws *)"]
  }
}
```

Put that in `~/.claude/settings.json` to cover every project, or in a project's
`.claude/settings.json`. A deny rule beats any allow rule, is matched against
each subcommand of a pipeline separately, and is matched through a leading
environment assignment, so `AWS_PROFILE=prod aws s3 rm ...` is blocked as well.
Other spellings are not: `/opt/homebrew/bin/aws` and `sh -c 'aws ...'` both get
through. It routes the agent to the proxy; it is not a boundary. The boundary
is the role the credentials carry.

### Agent skill

`skills/awsmcproxy` tells an agent how to drive the proxy: list the profiles
before the first call, ask when more than one name fits, and read an
`AccessDenied` as the answer rather than something to route around. Install it
with the [skills CLI](https://github.com/vercel-labs/skills):

```
npx skills add winebarrel/awsmcproxy          # into ./.claude/skills
npx skills add winebarrel/awsmcproxy -g       # into ~/.claude/skills
```

Or copy `skills/awsmcproxy` into `.claude/skills/` by hand. Either way Claude
Code picks it up on the next start.

## How it works

- On startup the proxy connects to the AWS MCP Server and lists its tools. Any
  identity will do, so it tries the default credential chain first and then each
  profile in turn.
- Each tool is re-registered with a required `profile` string argument. A
  proxy-native `list_profiles` tool is also added, which reads `~/.aws/config`
  and `~/.aws/credentials` on every call -- a profile added while the proxy is
  running is usable right away, so the valid names are deliberately not frozen
  into the tool schemas.
- On a tool call, the proxy strips the `profile` argument, looks up that
  profile's connection (opening it lazily, then caching it), and forwards the
  call over it.
- Every request is signed with SigV4 using credentials retrieved from the
  profile at request time, and carries that profile's region in the MCP `_meta`
  field as `AWS_REGION`, which is the default region for the AWS operations the
  server runs. A failed call drops the connection, so the next one reconnects
  with freshly retrieved credentials.

Profile names are read the way aws-sdk-go-v2 reads them, so a name from
`list_profiles` is always a name the SDK accepts: in `~/.aws/config` a
non-default profile needs the `profile ` prefix, while in `~/.aws/credentials`
it must not have one.
