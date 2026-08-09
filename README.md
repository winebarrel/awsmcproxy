# awsmcproxy

[![CI](https://github.com/winebarrel/awsmcproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/winebarrel/awsmcproxy/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/winebarrel/awsmcproxy/branch/main/graph/badge.svg)](https://codecov.io/gh/winebarrel/awsmcproxy)

A multi-profile proxy for the [AWS MCP Server](https://docs.aws.amazon.com/agent-toolkit/latest/userguide/getting-started-aws-mcp-server.html).

The AWS MCP Server authenticates each request with SigV4, so one connection
carries one AWS identity. `awsmcproxy` opens a connection per profile, mirrors
the server's tools, and adds a `profile` argument to each tool. When a tool is
called, the proxy signs the request with that profile's credentials.

```
Claude Code ──stdio──▶ awsmcproxy ──┬─ profile=dev  ─SigV4(my-dev)──▶ https://aws-mcp.us-east-1.api.aws/mcp
                                    └─ profile=prod ─SigV4(my-prod)─▶ https://aws-mcp.us-east-1.api.aws/mcp
```

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

## Configuration

Create a YAML config file. Values are passed through `os.ExpandEnv`, so values
can be referenced as `${ENV_VAR}` instead of being written literally.

```yaml
# awsmcproxy.yml

# Optional. Default: https://aws-mcp.us-east-1.api.aws/mcp
# endpoint: https://aws-mcp.us-east-1.api.aws/mcp

profiles:
  - name: dev
    aws_profile: my-dev
    region: us-east-1
  - name: prod
    aws_profile: my-prod
    region: ap-northeast-1
```

- `aws_profile` selects the profile in `~/.aws/config` used to sign the
  profile's requests.
- `region` is sent as the `AWS_REGION` metadata entry, which is the default
  region for the AWS operations the server runs. It is unrelated to the region
  the request is signed for, which comes from the endpoint.
- `endpoint` overrides the endpoint for a single profile.
- `metadata` adds `_meta` entries, per profile or globally.

`service` and `signing_region` override the SigV4 service and region, which are
otherwise inferred from the endpoint hostname. They are only needed for an
endpoint whose name does not identify them.

See [awsmcproxy.example.yml](awsmcproxy.example.yml) for the full file.

## Usage

```
Usage: awsmcproxy --config=STRING [flags]

Flags:
  -h, --help             Show help.
  -c, --config=STRING    Config file path ($AWSMCPROXY_CONFIG).
      --version
```

### Claude Code

Register it as an MCP server:

```json
{
  "mcpServers": {
    "awsmcproxy": {
      "command": "awsmcproxy",
      "args": ["--config", "/path/to/awsmcproxy.yml"]
    }
  }
}
```

## How it works

- On startup the proxy connects to the AWS MCP Server as the first profile and
  lists its tools.
- Each tool is re-registered with a required `profile` string argument
  (enumerated over the configured profile names). A proxy-native `list_profiles`
  tool is also added.
- On a tool call, the proxy strips the `profile` argument, looks up that
  profile's connection (opening it lazily, then caching it), and forwards the
  call over it.
- Every request is signed with SigV4 using credentials retrieved from the
  profile at request time, and carries the profile's `AWS_REGION` in the MCP
  `_meta` field. A failed call drops the connection, so the next one
  reconnects with freshly retrieved credentials.
