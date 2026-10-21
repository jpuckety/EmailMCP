# email-mcp CDK

Env stack (`EmailMcpStack`) for ECS Fargate + CodeDeploy blue/green (`email-mcp`)
and a rolling IMAP IDLE notify service (`email-mcp-notify`). MCPCICD deploys this
into Dev and Prod; do not `cdk deploy` from a laptop except for break-glass.

MCPCICD CodeDeploy still updates only `email-mcp`. `CdkDev`/`CdkProd` omit
`imageUri`. Dev has no Promote stage (`CdkDev` runs before Build); the Prod
order is `CdkProd` → `Promote` → `DeployProd`: `CdkProd` runs before Promote,
and `DeployProd` CodeDeploys only `email-mcp`. The notify image is a provisioning-time
GetAtt of SSM `/email-mcp/notify/image-uri` (busybox placeholder until you
write a digest). The watcher is **not** live automatically after Build or
Promote.

After Dev Build or Prod Promote, write the digest from that environment's ECR
account. For Prod, update SSM **after** Promote with the promoted Prod-account
digest; the preceding `CdkProd` cannot pick it up, and `DeployProd` does not
roll the notify service:

```bash
aws ssm put-parameter --name /email-mcp/notify/image-uri --type String --overwrite \
  --value "$ACCOUNT.dkr.ecr.$REGION.amazonaws.com/email-mcp@sha256:$DIGEST"
# Immediate roll: copy the current email-mcp-notify revision and change ONLY
# the image of both email-mcp-notify and volume-init. Keep the bootstrap
# entryPoint ['sh','-c'] and command (it execs /app/emailmcp -transport notify
# when present). Do not set command to ['-transport','notify'] while entryPoint
# is still sh -c — that crash-loops.
# --force-new-deployment alone does not replace a baked-in URI.
# Break-glass redeploy must include the same environment context MCPCICD uses:
npx cdk deploy EmailMcpStack \
  -c hostname=<MCPCICD-hostname> \
  -c pipelineAccount=<pipeline-account-id> \
  -c containerEnv='<same JSON passed by MCPCICD>' \
  -c hostedZoneName=<hosted-zone-name> \
  -c hostedZoneId=<hosted-zone-id> \
  -c imageUri=<environment-account-ecr-uri@sha256:digest>
```

Use the same `hostname`, `pipelineAccount`, `containerEnv`, and
`hostedZoneName`/`hostedZoneId` values MCPCICD passes for that environment.
Omitting `pipelineAccount` can remove pipeline SSM parameters and role outputs;
omitting `containerEnv` can drop environment-specific task settings. This
redeploy overwrites SSM with `imageUri` and is not a rollback-safe shortcut.

The next env-stack deploy re-reads SSM and updates the notify task definition
image if the parameter changed. A bad digest can fail the notify circuit
breaker and roll back the whole `EmailMcpStack` (HTTP included) until SSM is
fixed.

```bash
./run.sh synth
# or
cd cdk && npm test
```
