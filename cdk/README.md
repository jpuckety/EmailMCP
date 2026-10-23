# email-mcp CDK

Env stack (`EmailMcpStack`) for ECS Fargate + CodeDeploy blue/green for both
`email-mcp` (public ALB) and `email-mcp-notify` (private listener on port 8081).
MCPCICD deploys this into Dev and Prod; do not `cdk deploy` from a laptop except
for break-glass.

`CdkDev`/`CdkProd` omit `imageUri`, so both task families start as the busybox
`/health` placeholder. Dev has no Promote stage (`CdkDev` runs before Build);
the Prod order is `CdkProd` → `Promote` → `DeployProd`. `DeployDev`/`DeployProd`
CodeDeploy **both** services. `prepare-taskdef.js` accepts an optional family
argument; `email-mcp-notify` sets `command` to `-transport notify`. The first
env-stack deploy after switching notify to CodeDeploy deletes the existing
rolling `email-mcp-notify` service before creating the blue/green replacement
(ECS service names cannot be reused while the old service is still draining).

```bash
./run.sh synth
# or
cd cdk && npm test
```
