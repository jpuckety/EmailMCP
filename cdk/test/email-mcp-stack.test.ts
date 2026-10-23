import * as fs from 'fs';
import * as path from 'path';
import * as cdk from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import {
  APP_CONTAINER_NAME,
  APP_CONTAINER_PORT,
  APP_HEALTH_CHECK_PATH,
  EmailMcpStack,
  CODEDEPLOY_APPLICATION_NAME,
  ECR_REPOSITORY_NAME,
  ECR_PUSH_ROLE_NAME,
  NOTIFY_CODEDEPLOY_APPLICATION_NAME,
  NOTIFY_CODEDEPLOY_DEPLOYMENT_GROUP_NAME,
  NOTIFY_CONTAINER_NAME,
  NOTIFY_LISTENER_PORT,
  NOTIFY_SERVICE_NAME,
  NOTIFY_TASK_CPU,
  NOTIFY_TASK_FAMILY,
  NOTIFY_TASK_MEMORY_MIB,
  PIPELINE_DEPLOY_ROLE_NAME,
  PLACEHOLDER_APP_COMMAND,
  PLACEHOLDER_IMAGE,
  TASK_FAMILY,
} from '../lib/email-mcp-stack';

const PIPELINE_ACCOUNT = '111111111111';
const ENV_ACCOUNT = '222222222222';
const REGION = 'us-east-1';

function stackOf(props: Partial<ConstructorParameters<typeof EmailMcpStack>[2]> = {}): EmailMcpStack {
  const app = new cdk.App();
  return new EmailMcpStack(app, 'Test', {
    env: { account: ENV_ACCOUNT, region: REGION },
    allowInsecureHttp: true,
    pipelineAccount: PIPELINE_ACCOUNT,
    ...props,
  });
}

function synth(props: Partial<ConstructorParameters<typeof EmailMcpStack>[2]> = {}): Template {
  return Template.fromStack(stackOf(props));
}

function taskDefByFamily(template: Template, family: string): Record<string, any> {
  const defs = Object.values(template.findResources('AWS::ECS::TaskDefinition')) as Array<{
    Properties: { Family?: string };
  }>;
  const found = defs.find((def) => def.Properties.Family === family);
  expect(found).toBeDefined();
  return found as Record<string, any>;
}

function containerByName(taskDef: Record<string, any>, name: string): Record<string, any> {
  const found = (taskDef.Properties.ContainerDefinitions as Array<{ Name: string }>).find(
    (c) => c.Name === name,
  );
  expect(found).toBeDefined();
  return found as Record<string, any>;
}

function envMap(container: Record<string, any>): Record<string, string> {
  return Object.fromEntries(
    (container.Environment as Array<{ Name: string; Value: string }>).map((entry) => [entry.Name, entry.Value]),
  );
}

function parseAwsSdkCall(value: unknown): {
  service?: string;
  action?: string;
  parameters?: unknown;
  ignoreErrorCodesMatching?: string;
  logApiResponseData?: boolean;
  outputPaths?: string[];
} {
  if (typeof value === 'string') {
    return JSON.parse(value) as {
      service?: string;
      action?: string;
      parameters?: unknown;
      ignoreErrorCodesMatching?: string;
      logApiResponseData?: boolean;
      outputPaths?: string[];
    };
  }
  return (value ?? {}) as {
    service?: string;
    action?: string;
    parameters?: unknown;
    ignoreErrorCodesMatching?: string;
    logApiResponseData?: boolean;
    outputPaths?: string[];
  };
}

function awsSdkCallByAction(
  template: Template,
  createAction: string,
): { create: ReturnType<typeof parseAwsSdkCall>; update: ReturnType<typeof parseAwsSdkCall> } {
  const resources = template.findResources('Custom::AWS');
  for (const resource of Object.values(resources) as Array<{ Properties?: { Create?: unknown; Update?: unknown } }>) {
    const create = parseAwsSdkCall(resource.Properties?.Create);
    if (create.action === createAction) {
      return { create, update: parseAwsSdkCall(resource.Properties?.Update) };
    }
  }
  throw new Error(`no Custom::AWS resource with Create action ${createAction}`);
}

function customResourceByName(template: Template, name: string): Record<string, any> {
  const resources = template.findResources('Custom::AWS') as Record<string, Record<string, any>>;
  const found = Object.entries(resources).find(([logicalId]) => logicalId.includes(name))?.[1];
  expect(found).toBeDefined();
  return found ?? {};
}

describe('EmailMcpStack', () => {
  test('uses CODE_DEPLOY, two target groups, ECR, and no DockerImageAsset', () => {
    const template = synth();

    template.hasResourceProperties('AWS::ECS::Service', {
      DeploymentController: { Type: 'CODE_DEPLOY' },
      DesiredCount: 1,
      LaunchType: 'FARGATE',
      NetworkConfiguration: {
        AwsvpcConfiguration: {
          AssignPublicIp: 'DISABLED',
        },
      },
    });
    template.hasResourceProperties('AWS::ECS::Service', {
      TaskDefinition: TASK_FAMILY,
    });

    template.resourceCountIs('AWS::ElasticLoadBalancingV2::TargetGroup', 4);
    template.hasResourceProperties('AWS::ElasticLoadBalancingV2::TargetGroup', {
      HealthCheckPath: APP_HEALTH_CHECK_PATH,
      Port: APP_CONTAINER_PORT,
      TargetType: 'ip',
    });

    const json = JSON.stringify(template.toJSON());
    expect(json).toContain(ECR_REPOSITORY_NAME);
    expect(json).not.toMatch(/"AWS::ECR::Repository"/);

    template.hasResourceProperties('AWS::CodeDeploy::Application', {
      ApplicationName: CODEDEPLOY_APPLICATION_NAME,
      ComputePlatform: 'ECS',
    });
    template.hasResourceProperties('AWS::CodeDeploy::Application', {
      ApplicationName: NOTIFY_CODEDEPLOY_APPLICATION_NAME,
      ComputePlatform: 'ECS',
    });
    template.resourceCountIs('AWS::CodeDeploy::Application', 2);
    template.resourceCountIs('AWS::CodeDeploy::DeploymentGroup', 2);

    expect(json).not.toMatch(/DockerImageAsset/);
    expect(json).not.toMatch(/aws:cdk:path:.+ImageAsset/);
    expect(json).toContain(PLACEHOLDER_IMAGE);
    expect(json).toContain(APP_CONTAINER_NAME);
  });

  test('retains DynamoDB, KMS, and WAF association on the ALB', () => {
    const template = synth();

    template.resourceCountIs('AWS::DynamoDB::Table', 3);
    template.hasResourceProperties('AWS::KMS::Alias', { AliasName: 'alias/email-mcp' });
    template.hasResourceProperties('AWS::WAFv2::WebACL', { Name: 'email-mcp-web-acl', Scope: 'REGIONAL' });
    template.resourceCountIs('AWS::WAFv2::WebACLAssociation', 1);
  });

  test('task definition uses family, app container, sidecar, and read-only root', () => {
    const template = synth();

    template.hasResourceProperties('AWS::ECS::TaskDefinition', {
      Family: TASK_FAMILY,
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: APP_CONTAINER_NAME,
          PortMappings: Match.arrayWith([Match.objectLike({ ContainerPort: APP_CONTAINER_PORT })]),
          ReadonlyRootFilesystem: true,
        }),
        Match.objectLike({
          Name: 'volume-init',
          Essential: false,
          ReadonlyRootFilesystem: true,
        }),
      ]),
    });
  });

  test('placeholder app container stays running and answers the ALB health check', () => {
    const template = synth();

    template.hasResourceProperties('AWS::ECS::TaskDefinition', {
      ContainerDefinitions: Match.arrayWith([
        Match.objectLike({
          Name: APP_CONTAINER_NAME,
          Essential: true,
          Image: PLACEHOLDER_IMAGE,
          EntryPoint: ['sh', '-c'],
          Command: Match.arrayWith([
            Match.stringLikeRegexp('httpd'),
          ]),
        }),
      ]),
    });

    const app = containerByName(taskDefByFamily(template, TASK_FAMILY), APP_CONTAINER_NAME);
    expect(app.Command.join(' ')).toMatch(/8080/);
    expect(app.Command.join(' ')).toMatch(/health/);
  });

  test('real imageUri does not keep the placeholder listen command', () => {
    const template = synth({
      imageUri: '222222222222.dkr.ecr.us-east-1.amazonaws.com/email-mcp@sha256:abc',
    });
    const app = containerByName(taskDefByFamily(template, TASK_FAMILY), APP_CONTAINER_NAME);
    expect(app.Command).toBeUndefined();
    expect(app.EntryPoint).toBeUndefined();
  });

  test('merges pipeline containerEnv onto the app container', () => {
    const app = new cdk.App({
      context: {
        containerEnv: {
          OAUTH_REDIRECT_ALLOWLIST: 'https://www.cursor.com/agents/mcp/oauth/callback',
          OAUTH_ALLOW_KNOWN_CLIENTS: 'true',
        },
      },
    });
    const template = Template.fromStack(
      new EmailMcpStack(app, 'Test', {
        env: { account: ENV_ACCOUNT, region: REGION },
        allowInsecureHttp: true,
        pipelineAccount: PIPELINE_ACCOUNT,
      }),
    );
    const container = containerByName(taskDefByFamily(template, TASK_FAMILY), APP_CONTAINER_NAME);
    const env = envMap(container);
    expect(env.OAUTH_REDIRECT_ALLOWLIST).toBe('https://www.cursor.com/agents/mcp/oauth/callback');
    expect(env.OAUTH_ALLOW_KNOWN_CLIENTS).toBe('true');
    expect(env.EMAILMCP_KMS_KEY_ID).toBeDefined();
  });

  test('pipeline roles are imported by name without creating them', () => {
    const template = synth();
    const roles = Object.values(template.findResources('AWS::IAM::Role'));
    expect(roles.some((role) => role.Properties?.RoleName === ECR_PUSH_ROLE_NAME)).toBe(false);
    expect(roles.some((role) => role.Properties?.RoleName === PIPELINE_DEPLOY_ROLE_NAME)).toBe(false);
    const json = JSON.stringify(template.toJSON());
    expect(json).toContain(ECR_PUSH_ROLE_NAME);
    expect(json).toContain(PIPELINE_DEPLOY_ROLE_NAME);
    expect(json).toContain('/email-mcp/pipeline/ecr-push-role-arn');
    expect(json).toContain('/email-mcp/pipeline/pipeline-deploy-role-arn');
  });

  test('does not attach CloudFormation policies to the shared pipeline roles', () => {
    const template = synth();
    const policies = JSON.stringify(template.findResources('AWS::IAM::Policy'));
    expect(policies).not.toContain(ECR_PUSH_ROLE_NAME);
    expect(policies).not.toContain(PIPELINE_DEPLOY_ROLE_NAME);
    expect(policies).not.toContain('EcrPushRolePolicy');
  });

  test('container name, port, and health path match the Dockerfile contract', () => {
    expect(APP_CONTAINER_NAME).toBe('email-mcp');
    expect(APP_CONTAINER_PORT).toBe(8080);
    expect(APP_HEALTH_CHECK_PATH).toBe('/health');
  });

  test('creates KMS-encrypted NotifyState table with applicationId/folderKey keys', () => {
    const template = synth();
    template.hasResourceProperties('AWS::DynamoDB::Table', {
      KeySchema: [
        { AttributeName: 'applicationId', KeyType: 'HASH' },
        { AttributeName: 'folderKey', KeyType: 'RANGE' },
      ],
      BillingMode: 'PAY_PER_REQUEST',
      SSESpecification: {
        SSEEnabled: true,
        SSEType: 'KMS',
        KMSMasterKeyId: Match.objectLike({ 'Fn::GetAtt': Match.arrayWith([Match.stringLikeRegexp('DataKey')]) }),
      },
    });
    const json = JSON.stringify(template.toJSON());
    expect(json).toContain('/emailmcp/notify-state-table/name');
    expect(JSON.stringify(template.findResources('AWS::IAM::Policy'))).toContain('NotifyStateTable');
  });

  test('email-mcp-notify is a CODE_DEPLOY blue/green service on a private ALB listener', () => {
    const template = synth();

    template.resourceCountIs('AWS::ECS::Service', 2);
    template.resourceCountIs('AWS::CodeDeploy::DeploymentGroup', 2);
    template.hasResourceProperties('AWS::ECS::Service', {
      ServiceName: NOTIFY_SERVICE_NAME,
      DesiredCount: 1,
      LaunchType: 'FARGATE',
      DeploymentController: { Type: 'CODE_DEPLOY' },
      EnableExecuteCommand: false,
      TaskDefinition: NOTIFY_TASK_FAMILY,
      NetworkConfiguration: {
        AwsvpcConfiguration: {
          AssignPublicIp: 'DISABLED',
        },
      },
    });
    template.hasResourceProperties('AWS::ECS::Service', {
      DeploymentController: { Type: 'CODE_DEPLOY' },
      TaskDefinition: TASK_FAMILY,
    });
    template.hasResourceProperties('AWS::ElasticLoadBalancingV2::Listener', {
      Port: NOTIFY_LISTENER_PORT,
      Protocol: 'HTTP',
    });
    template.resourceCountIs('Custom::NotifyServiceCleanup', 1);
    template.hasResourceProperties('Custom::NotifyServiceCleanup', {
      ServiceName: NOTIFY_SERVICE_NAME,
      Cluster: Match.objectLike({ Ref: Match.stringLikeRegexp('Cluster') }),
    });
    const notifySvc = Object.values(template.findResources('AWS::ECS::Service')).find(
      (svc: { Properties?: { ServiceName?: string } }) => svc.Properties?.ServiceName === NOTIFY_SERVICE_NAME,
    ) as { DependsOn?: string[] } | undefined;
    expect(notifySvc).toBeDefined();
    expect(JSON.stringify(notifySvc?.DependsOn ?? [])).toMatch(/NotifyServiceCleanup/);

    template.hasResourceProperties('AWS::CodeDeploy::DeploymentGroup', {
      DeploymentGroupName: NOTIFY_CODEDEPLOY_DEPLOYMENT_GROUP_NAME,
    });
  });

  test('notify task uses family, health check, table env, and no Google secrets', () => {
    const template = synth();
    template.hasResourceProperties('AWS::ECS::TaskDefinition', {
      Family: NOTIFY_TASK_FAMILY,
      Cpu: String(NOTIFY_TASK_CPU),
      Memory: String(NOTIFY_TASK_MEMORY_MIB),
    });

    const notify = containerByName(taskDefByFamily(template, NOTIFY_TASK_FAMILY), NOTIFY_CONTAINER_NAME);
    expect(notify.Essential).toBe(true);
    expect(notify.Image).toBe(PLACEHOLDER_IMAGE);
    expect(notify.HealthCheck.Command.join(' ')).toMatch(/health/);
    expect(notify.Command).toEqual([PLACEHOLDER_APP_COMMAND]);
    expect(notify.Secrets).toBeUndefined();

    const env = envMap(notify);
    expect(env.EMAILMCP_TRANSPORT).toBe('notify');
    expect(env.EMAILMCP_USER_CONFIG_TABLE).toBeDefined();
    expect(env.EMAILMCP_NOTIFY_STATE_TABLE).toBeDefined();
    expect(env.EMAILMCP_KMS_KEY_ID).toBeDefined();
    expect(env.EMAILMCP_NOTIFY_OWNER).toBeUndefined();
    expect(env.GOOGLE_CLIENT_ID).toBeUndefined();
    expect(env.GOOGLE_CLIENT_SECRET).toBeUndefined();
  });

  test('real imageUri sets notify command to -transport notify', () => {
    const imageUri = '222222222222.dkr.ecr.us-east-1.amazonaws.com/email-mcp@sha256:abc';
    const template = synth({ imageUri });
    const notify = containerByName(taskDefByFamily(template, NOTIFY_TASK_FAMILY), NOTIFY_CONTAINER_NAME);
    expect(notify.Image).toBe(imageUri);
    expect(notify.Command).toEqual(['-transport', 'notify']);
    expect(notify.EntryPoint).toBeUndefined();
    expect(notify.Secrets).toBeUndefined();
  });

  test('notify placeholder image matches HTTP and is not an SSM parameter lookup', () => {
    const template = synth();
    const parameters = (template.toJSON().Parameters ?? {}) as Record<string, { Type?: string }>;
    for (const [name, param] of Object.entries(parameters)) {
      if (name === 'BootstrapVersion') {
        continue;
      }
      expect(param.Type).not.toMatch(/SSM::Parameter::Value/);
    }
    expect(JSON.stringify(template.findResources('Custom::AWS'))).not.toMatch(/NotifyImageUri/);

    const notify = containerByName(taskDefByFamily(template, NOTIFY_TASK_FAMILY), NOTIFY_CONTAINER_NAME);
    const volumeInit = containerByName(taskDefByFamily(template, NOTIFY_TASK_FAMILY), 'volume-init');
    expect(notify.Image).toBe(PLACEHOLDER_IMAGE);
    expect(volumeInit.Image).toBe(PLACEHOLDER_IMAGE);
  });

  test('PR workflow runs Go test and docker build', () => {
    const yml = fs.readFileSync(path.join(__dirname, '../../.github/workflows/pr-ci.yml'), 'utf8');
    expect(yml).toContain('go test');
    expect(yml).toContain('docker build');
  });
});
