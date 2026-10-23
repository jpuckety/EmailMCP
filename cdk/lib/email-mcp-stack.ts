import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as ecr from 'aws-cdk-lib/aws-ecr';
import * as dynamodb from 'aws-cdk-lib/aws-dynamodb';
import * as kms from 'aws-cdk-lib/aws-kms';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as ssm from 'aws-cdk-lib/aws-ssm';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as acm from 'aws-cdk-lib/aws-certificatemanager';
import * as elbv2 from 'aws-cdk-lib/aws-elasticloadbalancingv2';
import * as route53 from 'aws-cdk-lib/aws-route53';
import * as route53Targets from 'aws-cdk-lib/aws-route53-targets';
import * as wafv2 from 'aws-cdk-lib/aws-wafv2';
import * as codedeploy from 'aws-cdk-lib/aws-codedeploy';

/** Application container name; must match Dockerfile, appspec.yaml, and CodeDeploy. */
export const APP_CONTAINER_NAME = 'email-mcp';
/** Application container port; must match Dockerfile EXPOSE and appspec.yaml. */
export const APP_CONTAINER_PORT = 8080;
/** ALB / Docker health check path. */
export const APP_HEALTH_CHECK_PATH = '/health';
export const TASK_FAMILY = 'email-mcp';
export const NOTIFY_TASK_FAMILY = 'email-mcp-notify';
export const NOTIFY_SERVICE_NAME = 'email-mcp-notify';
export const NOTIFY_CONTAINER_NAME = 'email-mcp-notify';
/** Private ALB listener used only for CodeDeploy blue/green health (not public). */
export const NOTIFY_LISTENER_PORT = 8081;
export const NOTIFY_CODEDEPLOY_APPLICATION_NAME = 'email-mcp-notify';
export const NOTIFY_CODEDEPLOY_DEPLOYMENT_GROUP_NAME = 'email-mcp-notify';
/** CPU/memory for IMAP IDLE (up to 8 folders per account). */
export const NOTIFY_TASK_CPU = 512;
export const NOTIFY_TASK_MEMORY_MIB = 1024;
export const ECR_REPOSITORY_NAME = 'email-mcp';
export const ECS_CLUSTER_NAME = 'email-mcp';
export const ECS_SERVICE_NAME = 'email-mcp';
export const CODEDEPLOY_APPLICATION_NAME = 'email-mcp';
export const CODEDEPLOY_DEPLOYMENT_GROUP_NAME = 'email-mcp';
export const ECR_PUSH_ROLE_NAME = 'BroadWorksMcpEcrPushRole';
export const PIPELINE_DEPLOY_ROLE_NAME = 'BroadWorksMcpPipelineDeployRole';
/** Public image used until CodeDeploy rolls the real digest. Must provide `sh`/`chown` for volume-init and `httpd` so the essential container stays up and answers ALB `/health`. */
export const PLACEHOLDER_IMAGE = 'public.ecr.aws/docker/library/busybox:1.36';
/** Foreground httpd on the writable `/tmp` volume. Stripped by prepare-taskdef before CodeDeploy. */
export const PLACEHOLDER_APP_COMMAND =
  'mkdir -p /tmp/www && printf \'ok\\n\' > /tmp/www/health && exec httpd -f -p 8080 -h /tmp/www';

export interface EmailMcpStackProps extends cdk.StackProps {
  readonly hostname?: string;
  readonly certificateArn?: string;
  readonly hostedZoneName?: string;
  readonly hostedZoneId?: string;
  readonly allowInsecureHttp?: boolean;
  readonly pipelineAccount?: string;
  readonly imageUri?: string;
}

/**
 * Deploys email-mcp on ECS Fargate behind an (HTTPS) Application Load Balancer with CodeDeploy
 * blue/green, a second CodeDeploy blue/green Fargate service for IMAP IDLE notifications, an ECR
 * repository import, DynamoDB tables encrypted by a customer-managed KMS key, a task IAM role
 * granting scoped KMS + DynamoDB access, and SSM SecureString-backed secrets.
 */
export class EmailMcpStack extends cdk.Stack {
  public readonly repository: ecr.IRepository;
  public readonly cluster: ecs.Cluster;
  public readonly service: ecs.FargateService;
  public readonly notifyService: ecs.FargateService;
  public readonly loadBalancer: elbv2.ApplicationLoadBalancer;

  constructor(scope: Construct, id: string, props: EmailMcpStackProps = {}) {
    super(scope, id, props);

    const applicationId = this.node.tryGetContext('applicationId') ?? process.env.APPLICATION_ID ?? 'email-mcp';
    const extraContainerEnv = parseContainerEnvContext(this);
    const oauthRedirectAllowlist =
      extraContainerEnv.OAUTH_REDIRECT_ALLOWLIST ??
      this.node.tryGetContext('oauthRedirectAllowlist') ??
      process.env.OAUTH_REDIRECT_ALLOWLIST ??
      '';
    const ssmNames = this.node.tryGetContext('ssm') ?? {};

    const hostname: string | undefined = props.hostname ?? this.node.tryGetContext('hostname');
    const pipelineAccount: string | undefined =
      props.pipelineAccount ?? this.node.tryGetContext('pipelineAccount') ?? process.env.PIPELINE_ACCOUNT;
    const imageUri: string =
      props.imageUri ?? this.node.tryGetContext('imageUri') ?? process.env.IMAGE_URI ?? PLACEHOLDER_IMAGE;

    const hostedZoneId: string | undefined = props.hostedZoneId ?? this.node.tryGetContext('hostedZoneId');
    const hostedZoneName: string | undefined =
      props.hostedZoneName ??
      this.node.tryGetContext('hostedZoneName') ??
      (hostname && hostname.includes('.') ? hostname.substring(hostname.indexOf('.') + 1) : undefined);

    let hostedZone: route53.IHostedZone | undefined;
    if (hostname && hostedZoneName) {
      hostedZone = hostedZoneId
        ? route53.HostedZone.fromHostedZoneAttributes(this, 'HostedZone', {
            hostedZoneId,
            zoneName: hostedZoneName,
          })
        : route53.HostedZone.fromLookup(this, 'HostedZone', { domainName: hostedZoneName });
    }

    const natEip = new ec2.CfnEIP(this, 'NatGatewayEip', {
      domain: 'vpc',
      tags: [{ key: 'Name', value: 'email-mcp-nat' }],
    });

    const natGatewayProvider = ec2.NatProvider.gateway({
      eipAllocationIds: [natEip.attrAllocationId],
    });

    const vpc = new ec2.Vpc(this, 'Vpc', {
      maxAzs: 2,
      natGateways: 1,
      natGatewayProvider,
      subnetConfiguration: [
        {
          name: 'public',
          subnetType: ec2.SubnetType.PUBLIC,
          cidrMask: 24,
        },
        {
          name: 'private',
          subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS,
          cidrMask: 24,
        },
      ],
    });

    const dataKey = new kms.Key(this, 'DataKey', {
      alias: 'alias/email-mcp',
      enableKeyRotation: true,
      description: 'email-mcp: encrypts DynamoDB tables and email passwords',
    });

    const sessionsTable = new dynamodb.Table(this, 'SessionsTable', {
      partitionKey: { name: 'pk', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      encryption: dynamodb.TableEncryption.CUSTOMER_MANAGED,
      encryptionKey: dataKey,
      timeToLiveAttribute: 'ttl',
      pointInTimeRecoverySpecification: {
        pointInTimeRecoveryEnabled: true,
      },
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    sessionsTable.addGlobalSecondaryIndex({
      indexName: 'refresh-index',
      partitionKey: { name: 'refreshToken', type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });

    const userConfigTable = new dynamodb.Table(this, 'UserConfigTable', {
      partitionKey: { name: 'applicationId', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'userId', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      encryption: dynamodb.TableEncryption.CUSTOMER_MANAGED,
      encryptionKey: dataKey,
      pointInTimeRecoverySpecification: {
        pointInTimeRecoveryEnabled: true,
      },
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });

    const notifyStateTable = new dynamodb.Table(this, 'NotifyStateTable', {
      partitionKey: { name: 'applicationId', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'folderKey', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      encryption: dynamodb.TableEncryption.CUSTOMER_MANAGED,
      encryptionKey: dataKey,
      pointInTimeRecoverySpecification: {
        pointInTimeRecoveryEnabled: true,
      },
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });

    const taskRole = new iam.Role(this, 'TaskRole', {
      assumedBy: new iam.ServicePrincipal('ecs-tasks.amazonaws.com'),
      description: 'email-mcp task role: scoped DynamoDB + KMS access',
    });

    const googleClientId = ssm.StringParameter.fromSecureStringParameterAttributes(this, 'GoogleClientId', {
      parameterName: ssmNames.googleClientId ?? '/email-mcp/google-client-id',
    });
    const googleClientSecret = ssm.StringParameter.fromSecureStringParameterAttributes(this, 'GoogleClientSecret', {
      parameterName: ssmNames.googleClientSecret ?? '/email-mcp/google-client-secret',
    });

    this.repository = ecr.Repository.fromRepositoryName(this, 'Repository', ECR_REPOSITORY_NAME);

    const executionRole = new iam.Role(this, 'ExecutionRole', {
      assumedBy: new iam.ServicePrincipal('ecs-tasks.amazonaws.com'),
      description: 'email-mcp execution role: logs, SSM secrets, this account ECR only',
    });
    this.repository.grantPull(executionRole);
    executionRole.addToPrincipalPolicy(
      new iam.PolicyStatement({
        actions: ['ecr:GetAuthorizationToken'],
        resources: ['*'],
      }),
    );

    dataKey.addToResourcePolicy(
      new iam.PolicyStatement({
        sid: 'AllowCloudWatchLogs',
        principals: [new iam.ServicePrincipal(`logs.${this.region}.amazonaws.com`)],
        actions: [
          'kms:Encrypt*',
          'kms:Decrypt*',
          'kms:ReEncrypt*',
          'kms:GenerateDataKey*',
          'kms:Describe*',
        ],
        resources: ['*'],
        conditions: {
          ArnLike: {
            'kms:EncryptionContext:aws:logs:arn': `arn:${this.partition}:logs:${this.region}:${this.account}:log-group:*`,
          },
        },
      }),
    );

    const logGroup = new logs.LogGroup(this, 'LogGroup', {
      retention: logs.RetentionDays.ONE_MONTH,
      encryptionKey: dataKey,
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    logGroup.grantWrite(executionRole);

    this.cluster = new ecs.Cluster(this, 'Cluster', {
      vpc,
      clusterName: ECS_CLUSTER_NAME,
      containerInsightsV2: ecs.ContainerInsights.ENABLED,
    });

    const allowInsecureHttp: boolean =
      props.allowInsecureHttp ?? String(this.node.tryGetContext('allowInsecureHttp') ?? 'false') === 'true';

    let certificate: acm.ICertificate | undefined;
    if (props.certificateArn) {
      certificate = acm.Certificate.fromCertificateArn(this, 'Certificate', props.certificateArn);
    } else if (hostname) {
      certificate = new acm.Certificate(this, 'Certificate', {
        domainName: hostname,
        validation: hostedZone
          ? acm.CertificateValidation.fromDns(hostedZone)
          : acm.CertificateValidation.fromDns(),
      });
    } else if (!allowInsecureHttp) {
      throw new Error(
        'HTTPS is required: provide -c hostname=email.mcp.ecg.co (to create an ACM certificate) or ' +
          '-c certificateArn=arn:aws:acm:<region>:<acct>:certificate/<id>. For local/dev only, opt out ' +
          'of TLS with -c allowInsecureHttp=true.',
      );
    }

    const healthCheck: elbv2.HealthCheck = {
      path: APP_HEALTH_CHECK_PATH,
      healthyHttpCodes: '200',
      interval: cdk.Duration.seconds(30),
    };

    this.loadBalancer = new elbv2.ApplicationLoadBalancer(this, 'Alb', {
      vpc,
      internetFacing: true,
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
      idleTimeout: cdk.Duration.seconds(120),
    });

    const blueTargetGroup = new elbv2.ApplicationTargetGroup(this, 'BlueTargetGroup', {
      vpc,
      port: APP_CONTAINER_PORT,
      protocol: elbv2.ApplicationProtocol.HTTP,
      targetType: elbv2.TargetType.IP,
      healthCheck,
      deregistrationDelay: cdk.Duration.seconds(30),
    });
    const greenTargetGroup = new elbv2.ApplicationTargetGroup(this, 'GreenTargetGroup', {
      vpc,
      port: APP_CONTAINER_PORT,
      protocol: elbv2.ApplicationProtocol.HTTP,
      targetType: elbv2.TargetType.IP,
      healthCheck,
      deregistrationDelay: cdk.Duration.seconds(30),
    });

    let productionListener: elbv2.ApplicationListener;
    if (certificate) {
      this.loadBalancer.addListener('HttpListener', {
        port: 80,
        protocol: elbv2.ApplicationProtocol.HTTP,
        defaultAction: elbv2.ListenerAction.redirect({
          port: '443',
          protocol: 'HTTPS',
          permanent: true,
        }),
      });
      productionListener = this.loadBalancer.addListener('HttpsListener', {
        port: 443,
        protocol: elbv2.ApplicationProtocol.HTTPS,
        certificates: [certificate],
        defaultAction: elbv2.ListenerAction.forward([blueTargetGroup]),
      });
    } else {
      productionListener = this.loadBalancer.addListener('HttpListener', {
        port: 80,
        protocol: elbv2.ApplicationProtocol.HTTP,
        defaultAction: elbv2.ListenerAction.forward([blueTargetGroup]),
      });
    }

    const image = ecs.ContainerImage.fromRegistry(imageUri);

    const taskDefinition = new ecs.FargateTaskDefinition(this, 'TaskDef', {
      family: TASK_FAMILY,
      cpu: 512,
      memoryLimitMiB: 1024,
      taskRole,
      executionRole,
      runtimePlatform: {
        cpuArchitecture: ecs.CpuArchitecture.X86_64,
        operatingSystemFamily: ecs.OperatingSystemFamily.LINUX,
      },
    });

    const appLogDriver = ecs.LogDrivers.awsLogs({ streamPrefix: 'email-mcp', logGroup });
    const usingPlaceholder = imageUri === PLACEHOLDER_IMAGE;
    const publicBaseUrl = hostname ? `https://${hostname}` : '';
    const appContainer = taskDefinition.addContainer('App', {
      containerName: APP_CONTAINER_NAME,
      image,
      portMappings: [{ containerPort: APP_CONTAINER_PORT }],
      logging: appLogDriver,
      essential: true,
      ...(usingPlaceholder
        ? { entryPoint: ['sh', '-c'], command: [PLACEHOLDER_APP_COMMAND] }
        : {}),
      environment: {
        EMAILMCP_LISTEN_ADDR: `:${APP_CONTAINER_PORT}`,
        EMAILMCP_TRANSPORT: 'http',
        EMAILMCP_SESSION_TABLE: sessionsTable.tableName,
        EMAILMCP_USER_CONFIG_TABLE: userConfigTable.tableName,
        EMAILMCP_KMS_KEY_ID: dataKey.keyArn,
        APPLICATION_ID: applicationId,
        AWS_REGION: this.region,
        OAUTH_REDIRECT_ALLOWLIST: oauthRedirectAllowlist,
        PUBLIC_BASE_URL: publicBaseUrl,
        ...extraContainerEnv,
      },
      secrets: {
        GOOGLE_CLIENT_ID: ecs.Secret.fromSsmParameter(googleClientId),
        GOOGLE_CLIENT_SECRET: ecs.Secret.fromSsmParameter(googleClientSecret),
      },
    });

    taskDefinition.addVolume({ name: 'tmp' });
    taskDefinition.addVolume({ name: 'ssm-agent-state' });
    taskDefinition.addVolume({ name: 'ssm-agent-logs' });
    appContainer.addMountPoints(
      { sourceVolume: 'tmp', containerPath: '/tmp', readOnly: false },
      { sourceVolume: 'ssm-agent-state', containerPath: '/var/lib/amazon', readOnly: false },
      { sourceVolume: 'ssm-agent-logs', containerPath: '/var/log/amazon', readOnly: false },
    );

    const volumeInit = taskDefinition.addContainer('VolumeInit', {
      image,
      containerName: 'volume-init',
      user: 'root',
      essential: false,
      memoryReservationMiB: 64,
      entryPoint: ['sh', '-c'],
      command: [
        'set -e; ' +
          'chown 10001:10001 /tmp; ' +
          'chmod 1777 /tmp; ' +
          'mkdir -p /var/lib/amazon/ssm /var/log/amazon/ssm; ' +
          'chown -R 10001:10001 /var/lib/amazon /var/log/amazon',
      ],
      logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'volume-init', logGroup }),
    });
    volumeInit.addMountPoints(
      { sourceVolume: 'tmp', containerPath: '/tmp', readOnly: false },
      { sourceVolume: 'ssm-agent-state', containerPath: '/var/lib/amazon', readOnly: false },
      { sourceVolume: 'ssm-agent-logs', containerPath: '/var/log/amazon', readOnly: false },
    );
    appContainer.addContainerDependencies({
      container: volumeInit,
      condition: ecs.ContainerDependencyCondition.SUCCESS,
    });

    const cfnTaskDefinition = taskDefinition.node.defaultChild as ecs.CfnTaskDefinition;
    for (const index of [0, 1]) {
      cfnTaskDefinition.addPropertyOverride(`ContainerDefinitions.${index}.ReadonlyRootFilesystem`, true);
    }

    this.service = new ecs.FargateService(this, 'BlueGreenService', {
      cluster: this.cluster,
      serviceName: ECS_SERVICE_NAME,
      taskDefinition,
      desiredCount: 1,
      minHealthyPercent: 0,
      maxHealthyPercent: 200,
      assignPublicIp: false,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
      enableExecuteCommand: true,
      healthCheckGracePeriod: cdk.Duration.seconds(120),
      deploymentController: { type: ecs.DeploymentControllerType.CODE_DEPLOY },
    });
    this.service.attachToApplicationTargetGroup(blueTargetGroup);
    this.service.connections.allowFrom(this.loadBalancer, ec2.Port.tcp(APP_CONTAINER_PORT));

    const cfnService = this.service.node.defaultChild as ecs.CfnService;
    cfnService.taskDefinition = TASK_FAMILY;

    const application = new codedeploy.EcsApplication(this, 'CodeDeployApplication', {
      applicationName: CODEDEPLOY_APPLICATION_NAME,
    });
    new codedeploy.EcsDeploymentGroup(this, 'DeploymentGroup', {
      application,
      deploymentGroupName: CODEDEPLOY_DEPLOYMENT_GROUP_NAME,
      service: this.service,
      blueGreenDeploymentConfig: {
        blueTargetGroup,
        greenTargetGroup,
        listener: productionListener,
      },
      autoRollback: { failedDeployment: true },
      deploymentConfig: codedeploy.EcsDeploymentConfig.ALL_AT_ONCE,
    });

    const notifyBlueTargetGroup = new elbv2.ApplicationTargetGroup(this, 'NotifyBlueTargetGroup', {
      vpc,
      port: APP_CONTAINER_PORT,
      protocol: elbv2.ApplicationProtocol.HTTP,
      targetType: elbv2.TargetType.IP,
      healthCheck,
      deregistrationDelay: cdk.Duration.seconds(30),
    });
    const notifyGreenTargetGroup = new elbv2.ApplicationTargetGroup(this, 'NotifyGreenTargetGroup', {
      vpc,
      port: APP_CONTAINER_PORT,
      protocol: elbv2.ApplicationProtocol.HTTP,
      targetType: elbv2.TargetType.IP,
      healthCheck,
      deregistrationDelay: cdk.Duration.seconds(30),
    });
    const notifyListener = this.loadBalancer.addListener('NotifyListener', {
      port: NOTIFY_LISTENER_PORT,
      protocol: elbv2.ApplicationProtocol.HTTP,
      open: false,
      defaultAction: elbv2.ListenerAction.forward([notifyBlueTargetGroup]),
    });

    const notifyTaskDefinition = new ecs.FargateTaskDefinition(this, 'NotifyTaskDef', {
      family: NOTIFY_TASK_FAMILY,
      cpu: NOTIFY_TASK_CPU,
      memoryLimitMiB: NOTIFY_TASK_MEMORY_MIB,
      taskRole,
      executionRole,
      runtimePlatform: {
        cpuArchitecture: ecs.CpuArchitecture.X86_64,
        operatingSystemFamily: ecs.OperatingSystemFamily.LINUX,
      },
    });

    const notifyLogDriver = ecs.LogDrivers.awsLogs({ streamPrefix: 'email-mcp-notify', logGroup });
    const notifyHealthCheck: ecs.HealthCheck = {
      command: ['CMD-SHELL', `wget -qO- http://127.0.0.1:${APP_CONTAINER_PORT}${APP_HEALTH_CHECK_PATH} || exit 1`],
      interval: cdk.Duration.seconds(30),
      timeout: cdk.Duration.seconds(5),
      retries: 3,
      startPeriod: cdk.Duration.seconds(30),
    };
    const notifyContainer = notifyTaskDefinition.addContainer('NotifyApp', {
      containerName: NOTIFY_CONTAINER_NAME,
      image,
      portMappings: [{ containerPort: APP_CONTAINER_PORT }],
      logging: notifyLogDriver,
      essential: true,
      healthCheck: notifyHealthCheck,
      ...(usingPlaceholder
        ? { entryPoint: ['sh', '-c'], command: [PLACEHOLDER_APP_COMMAND] }
        : { command: ['-transport', 'notify'] }),
      environment: {
        EMAILMCP_LISTEN_ADDR: `:${APP_CONTAINER_PORT}`,
        EMAILMCP_TRANSPORT: 'notify',
        EMAILMCP_USER_CONFIG_TABLE: userConfigTable.tableName,
        EMAILMCP_NOTIFY_STATE_TABLE: notifyStateTable.tableName,
        EMAILMCP_KMS_KEY_ID: dataKey.keyArn,
        APPLICATION_ID: applicationId,
        AWS_REGION: this.region,
        ...extraContainerEnv,
      },
    });

    notifyTaskDefinition.addVolume({ name: 'tmp' });
    notifyContainer.addMountPoints({ sourceVolume: 'tmp', containerPath: '/tmp', readOnly: false });

    const notifyVolumeInit = notifyTaskDefinition.addContainer('NotifyVolumeInit', {
      image,
      containerName: 'volume-init',
      user: 'root',
      essential: false,
      memoryReservationMiB: 64,
      entryPoint: ['sh', '-c'],
      command: ['set -e; chown 10001:10001 /tmp; chmod 1777 /tmp'],
      logging: ecs.LogDrivers.awsLogs({ streamPrefix: 'notify-volume-init', logGroup }),
    });
    notifyVolumeInit.addMountPoints({ sourceVolume: 'tmp', containerPath: '/tmp', readOnly: false });
    notifyContainer.addContainerDependencies({
      container: notifyVolumeInit,
      condition: ecs.ContainerDependencyCondition.SUCCESS,
    });

    const cfnNotifyTaskDefinition = notifyTaskDefinition.node.defaultChild as ecs.CfnTaskDefinition;
    for (const index of [0, 1]) {
      cfnNotifyTaskDefinition.addPropertyOverride(`ContainerDefinitions.${index}.ReadonlyRootFilesystem`, true);
    }

    this.notifyService = new ecs.FargateService(this, 'NotifyBlueGreenService', {
      cluster: this.cluster,
      serviceName: NOTIFY_SERVICE_NAME,
      taskDefinition: notifyTaskDefinition,
      desiredCount: 1,
      minHealthyPercent: 0,
      maxHealthyPercent: 200,
      assignPublicIp: false,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
      enableExecuteCommand: false,
      healthCheckGracePeriod: cdk.Duration.seconds(120),
      deploymentController: { type: ecs.DeploymentControllerType.CODE_DEPLOY },
    });
    this.notifyService.attachToApplicationTargetGroup(notifyBlueTargetGroup);
    this.notifyService.connections.allowFrom(this.loadBalancer, ec2.Port.tcp(APP_CONTAINER_PORT));

    const cfnNotifyService = this.notifyService.node.defaultChild as ecs.CfnService;
    cfnNotifyService.taskDefinition = NOTIFY_TASK_FAMILY;

    const notifyApplication = new codedeploy.EcsApplication(this, 'NotifyCodeDeployApplication', {
      applicationName: NOTIFY_CODEDEPLOY_APPLICATION_NAME,
    });
    new codedeploy.EcsDeploymentGroup(this, 'NotifyDeploymentGroup', {
      application: notifyApplication,
      deploymentGroupName: NOTIFY_CODEDEPLOY_DEPLOYMENT_GROUP_NAME,
      service: this.notifyService,
      blueGreenDeploymentConfig: {
        blueTargetGroup: notifyBlueTargetGroup,
        greenTargetGroup: notifyGreenTargetGroup,
        listener: notifyListener,
      },
      autoRollback: { failedDeployment: true },
      deploymentConfig: codedeploy.EcsDeploymentConfig.ALL_AT_ONCE,
    });

    const wafManagedRuleExcludedPaths = ['/mcp', '/sse', '/oauth/register', '/oauth/authorize', '/oauth/token'];
    const notExcludedEndpoints: wafv2.CfnWebACL.StatementProperty = {
      notStatement: {
        statement: {
          orStatement: {
            statements: wafManagedRuleExcludedPaths.map((uriPathPrefix) => ({
              byteMatchStatement: {
                searchString: uriPathPrefix,
                fieldToMatch: { uriPath: {} },
                positionalConstraint: 'STARTS_WITH',
                textTransformations: [
                  { priority: 0, type: 'URL_DECODE' },
                  { priority: 1, type: 'LOWERCASE' },
                ],
              },
            })),
          },
        },
      },
    };

    const webAcl = new wafv2.CfnWebACL(this, 'WebAcl', {
      name: 'email-mcp-web-acl',
      scope: 'REGIONAL',
      defaultAction: { allow: {} },
      visibilityConfig: {
        cloudWatchMetricsEnabled: true,
        sampledRequestsEnabled: true,
        metricName: 'email-mcp-web-acl',
      },
      rules: [
        {
          name: 'AWSManagedRulesCommonRuleSet',
          priority: 0,
          overrideAction: { none: {} },
          statement: {
            managedRuleGroupStatement: {
              vendorName: 'AWS',
              name: 'AWSManagedRulesCommonRuleSet',
              scopeDownStatement: notExcludedEndpoints,
            },
          },
          visibilityConfig: {
            cloudWatchMetricsEnabled: true,
            sampledRequestsEnabled: true,
            metricName: 'CommonRuleSet',
          },
        },
        {
          name: 'AWSManagedRulesKnownBadInputsRuleSet',
          priority: 1,
          overrideAction: { none: {} },
          statement: {
            managedRuleGroupStatement: {
              vendorName: 'AWS',
              name: 'AWSManagedRulesKnownBadInputsRuleSet',
              scopeDownStatement: notExcludedEndpoints,
            },
          },
          visibilityConfig: {
            cloudWatchMetricsEnabled: true,
            sampledRequestsEnabled: true,
            metricName: 'KnownBadInputs',
          },
        },
        {
          name: 'RateLimitOauthRegister',
          priority: 2,
          action: { block: {} },
          statement: {
            rateBasedStatement: {
              limit: 100,
              evaluationWindowSec: 300,
              aggregateKeyType: 'IP',
              scopeDownStatement: {
                byteMatchStatement: {
                  searchString: '/oauth/register',
                  fieldToMatch: { uriPath: {} },
                  positionalConstraint: 'STARTS_WITH',
                  textTransformations: [{ priority: 0, type: 'LOWERCASE' }],
                },
              },
            },
          },
          visibilityConfig: {
            cloudWatchMetricsEnabled: true,
            sampledRequestsEnabled: true,
            metricName: 'RateLimitOauthRegister',
          },
        },
        {
          name: 'RateLimitOauthToken',
          priority: 3,
          action: { block: {} },
          statement: {
            rateBasedStatement: {
              limit: 100,
              evaluationWindowSec: 300,
              aggregateKeyType: 'IP',
              scopeDownStatement: {
                byteMatchStatement: {
                  searchString: '/oauth/token',
                  fieldToMatch: { uriPath: {} },
                  positionalConstraint: 'STARTS_WITH',
                  textTransformations: [{ priority: 0, type: 'LOWERCASE' }],
                },
              },
            },
          },
          visibilityConfig: {
            cloudWatchMetricsEnabled: true,
            sampledRequestsEnabled: true,
            metricName: 'RateLimitOauthToken',
          },
        },
        {
          name: 'RateLimitGeneral',
          priority: 4,
          action: { block: {} },
          statement: {
            rateBasedStatement: {
              limit: 2000,
              evaluationWindowSec: 300,
              aggregateKeyType: 'IP',
            },
          },
          visibilityConfig: {
            cloudWatchMetricsEnabled: true,
            sampledRequestsEnabled: true,
            metricName: 'RateLimitGeneral',
          },
        },
      ],
    });

    new wafv2.CfnWebACLAssociation(this, 'WebAclAssociation', {
      resourceArn: this.loadBalancer.loadBalancerArn,
      webAclArn: webAcl.attrArn,
    });

    const wafLogGroupName = 'aws-waf-logs-email-mcp';
    const wafLogGroup = new logs.LogGroup(this, 'WafLogGroup', {
      logGroupName: wafLogGroupName,
      retention: logs.RetentionDays.ONE_MONTH,
      encryptionKey: dataKey,
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });

    const wafLoggingConfiguration = new wafv2.CfnLoggingConfiguration(this, 'WebAclLoggingConfiguration', {
      resourceArn: webAcl.attrArn,
      logDestinationConfigs: [
        this.formatArn({
          service: 'logs',
          resource: 'log-group',
          resourceName: wafLogGroupName,
          arnFormat: cdk.ArnFormat.COLON_RESOURCE_NAME,
        }),
      ],
      redactedFields: [
        { singleHeader: { Name: 'authorization' } },
        { singleHeader: { Name: 'cookie' } },
      ],
    });
    wafLoggingConfiguration.node.addDependency(wafLogGroup);

    if (hostname && hostedZone) {
      const albAliasTarget = route53.RecordTarget.fromAlias(new route53Targets.LoadBalancerTarget(this.loadBalancer));
      new route53.ARecord(this, 'AliasRecord', {
        zone: hostedZone,
        recordName: hostname,
        target: albAliasTarget,
        comment: 'email-mcp: hostname -> ALB (IPv4)',
      });
      new route53.AaaaRecord(this, 'AliasRecordAAAA', {
        zone: hostedZone,
        recordName: hostname,
        target: albAliasTarget,
        comment: 'email-mcp: hostname -> ALB (IPv6)',
      });
    }

    sessionsTable.grantReadWriteData(taskRole);
    userConfigTable.grantReadWriteData(taskRole);
    notifyStateTable.grantReadWriteData(taskRole);
    dataKey.grantEncryptDecrypt(taskRole);

    new ssm.StringParameter(this, 'UserConfigTableNameParam', {
      parameterName: '/emailmcp/user-config-table/name',
      stringValue: userConfigTable.tableName,
      description: 'DynamoDB table name for EmailMCP per-user email account configurations',
    });
    new ssm.StringParameter(this, 'SessionTableNameParam', {
      parameterName: '/emailmcp/session-table/name',
      stringValue: sessionsTable.tableName,
      description: 'DynamoDB table name for EmailMCP OAuth sessions',
    });
    new ssm.StringParameter(this, 'NotifyStateTableNameParam', {
      parameterName: '/emailmcp/notify-state-table/name',
      stringValue: notifyStateTable.tableName,
      description: 'DynamoDB table name for EmailMCP IMAP IDLE leases and UID watermarks',
    });

    if (pipelineAccount) {
      // OrganizationStack creates these names with AdministratorAccess. Do not
      // attach extra AWS::IAM::Policy resources: BroadWorksMcpStack already
      // owns EcrPushRole/Policy (same generated name) and CloudFormation
      // rejects a second manager.
      const ecrPushRole = iam.Role.fromRoleName(this, 'EcrPushRole', ECR_PUSH_ROLE_NAME);
      const pipelineDeployRole = iam.Role.fromRoleName(this, 'PipelineDeployRole', PIPELINE_DEPLOY_ROLE_NAME);

      new ssm.StringParameter(this, 'EcrUriParameter', {
        parameterName: '/email-mcp/pipeline/ecr-uri',
        stringValue: this.repository.repositoryUri,
      });
      new ssm.StringParameter(this, 'EcrPushRoleArnParameter', {
        parameterName: '/email-mcp/pipeline/ecr-push-role-arn',
        stringValue: ecrPushRole.roleArn,
      });
      new ssm.StringParameter(this, 'PipelineDeployRoleArnParameter', {
        parameterName: '/email-mcp/pipeline/pipeline-deploy-role-arn',
        stringValue: pipelineDeployRole.roleArn,
      });

      new cdk.CfnOutput(this, 'EcrPushRoleArn', { value: ecrPushRole.roleArn });
      new cdk.CfnOutput(this, 'PipelineDeployRoleArn', { value: pipelineDeployRole.roleArn });
    } else {
      cdk.Annotations.of(this).addWarning(
        'pipelineAccount is not set: EcrPushRole and PipelineDeployRole were not imported. Pass ' +
          '-c pipelineAccount=<pipeline-aws-account-id> so MCPCICD can assume into this environment.',
      );
    }

    new cdk.CfnOutput(this, 'LoadBalancerDns', {
      value: this.loadBalancer.loadBalancerDnsName,
      description: 'Public DNS name of the MCP load balancer',
    });
    if (hostname && hostedZone) {
      new cdk.CfnOutput(this, 'PublicUrl', {
        value: `https://${hostname}`,
        description: 'Public URL served by the Route 53 alias record pointing at the ALB',
      });
    }
    new cdk.CfnOutput(this, 'NatGatewayEipAddress', {
      value: natEip.ref,
      description: 'Fixed Elastic IP for the NAT gateway (stable outbound public IP of the ECS tasks)',
    });
    new cdk.CfnOutput(this, 'SessionsTableName', { value: sessionsTable.tableName });
    new cdk.CfnOutput(this, 'UserConfigTableName', { value: userConfigTable.tableName });
    new cdk.CfnOutput(this, 'NotifyStateTableName', { value: notifyStateTable.tableName });
    new cdk.CfnOutput(this, 'KmsKeyId', { value: dataKey.keyId });
    new cdk.CfnOutput(this, 'EcrRepositoryUri', { value: this.repository.repositoryUri });
    new cdk.CfnOutput(this, 'ClusterName', { value: this.cluster.clusterName });
    new cdk.CfnOutput(this, 'ServiceName', { value: this.service.serviceName });
    new cdk.CfnOutput(this, 'NotifyServiceName', { value: this.notifyService.serviceName });
    new cdk.CfnOutput(this, 'CodeDeployApplicationName', { value: CODEDEPLOY_APPLICATION_NAME });
    new cdk.CfnOutput(this, 'CodeDeployDeploymentGroupName', { value: CODEDEPLOY_DEPLOYMENT_GROUP_NAME });
    new cdk.CfnOutput(this, 'NotifyCodeDeployApplicationName', { value: NOTIFY_CODEDEPLOY_APPLICATION_NAME });
    new cdk.CfnOutput(this, 'NotifyCodeDeployDeploymentGroupName', {
      value: NOTIFY_CODEDEPLOY_DEPLOYMENT_GROUP_NAME,
    });

    if (!certificate) {
      cdk.Annotations.of(this).addWarning(
        'allowInsecureHttp is set: the ALB listens on HTTP only. Never use this outside local/dev; provide ' +
          '-c hostname=email.mcp.ecg.co (to create a certificate) or -c certificateArn=... for HTTPS.',
      );
    }

    if (hostname && !hostedZone) {
      cdk.Annotations.of(this).addWarning(
        `No Route 53 hosted zone resolved for '${hostname}': DNS alias records were not created and the ` +
          'ACM certificate must be DNS-validated manually. Provide -c hostedZoneName=example.com (and ' +
          'optionally -c hostedZoneId=...) so the infrastructure can create the necessary DNS entries.',
      );
    }
  }
}

const CONTAINER_ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** MCPCICD CdkDev/CdkProd pass `-c containerEnv=<json>` for non-secret task env vars. */
function parseContainerEnvContext(scope: Construct): Record<string, string> {
  const raw = scope.node.tryGetContext('containerEnv');
  if (raw === undefined || raw === null || raw === '') {
    return {};
  }
  let record: unknown = raw;
  if (typeof record === 'string') {
    try {
      record = JSON.parse(record);
    } catch {
      throw new Error("context 'containerEnv' must be a JSON object of string values");
    }
  }
  if (!record || typeof record !== 'object' || Array.isArray(record)) {
    throw new Error("context 'containerEnv' must be an object of string values");
  }
  const out: Record<string, string> = {};
  for (const [name, entry] of Object.entries(record as Record<string, unknown>)) {
    if (!CONTAINER_ENV_NAME.test(name)) {
      throw new Error(`context 'containerEnv' has invalid environment variable name '${name}'`);
    }
    if (typeof entry !== 'string') {
      throw new Error(`context 'containerEnv.${name}' must be a string`);
    }
    out[name] = entry;
  }
  return out;
}
