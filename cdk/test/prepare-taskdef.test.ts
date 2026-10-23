import {
  appspecYaml,
  prepareTaskdef,
} from '../scripts/prepare-taskdef.js';

const liveDescribe = {
  taskDefinition: {
    taskDefinitionArn: 'arn:aws:ecs:us-east-1:222222222222:task-definition/email-mcp:17',
    revision: 17,
    status: 'ACTIVE',
    family: 'email-mcp',
    taskRoleArn: 'arn:aws:iam::222222222222:role/Task',
    executionRoleArn: 'arn:aws:iam::222222222222:role/Exec',
    requiresAttributes: [{ name: 'ecs.capability.execution-role-ecr-pull' }],
    compatibilities: ['EC2', 'FARGATE'],
    registeredAt: '2026-01-01T00:00:00.000Z',
    registeredBy: 'arn:aws:iam::222222222222:role/cdk',
    containerDefinitions: [
      {
        name: 'email-mcp',
        image: '222222222222.dkr.ecr.us-east-1.amazonaws.com/email-mcp@sha256:aaa',
        essential: true,
        entryPoint: ['sh', '-c'],
        command: ['mkdir -p /tmp/www && httpd -f -p 8080 -h /tmp/www'],
        environment: [{ name: 'EMAILMCP_TRANSPORT', value: 'http' }],
        secrets: [{ name: 'GOOGLE_CLIENT_SECRET', valueFrom: 'arn:aws:ssm:us-east-1:222222222222:parameter/x' }],
        mountPoints: [{ sourceVolume: 'tmp', containerPath: '/tmp' }],
      },
      {
        name: 'volume-init',
        image: 'public.ecr.aws/amazonlinux/amazonlinux:2023',
        essential: false,
        command: ['chown 10001:10001 /tmp'],
      },
    ],
    volumes: [{ name: 'tmp' }],
    cpu: '512',
    memory: '1024',
  },
};

describe('prepare-taskdef', () => {
  test('substitutes only the app container image and preserves the sidecar', () => {
    const td = prepareTaskdef(liveDescribe);

    expect(td.taskDefinitionArn).toBeUndefined();
    expect(td.revision).toBeUndefined();
    expect(td.status).toBeUndefined();
    expect(td.requiresAttributes).toBeUndefined();
    expect(td.compatibilities).toBeUndefined();
    expect(td.family).toBe('email-mcp');
    expect(td.cpu).toBe('512');
    expect(td.volumes).toEqual([{ name: 'tmp' }]);

    const app = td.containerDefinitions.find((c: { name: string }) => c.name === 'email-mcp');
    const init = td.containerDefinitions.find((c: { name: string }) => c.name === 'volume-init');
    expect(app.image).toBe('<IMAGE1_NAME>');
    expect(app.entryPoint).toBeUndefined();
    expect(app.command).toBeUndefined();
    expect(app.environment[0].value).toBe('http');
    expect(app.secrets).toHaveLength(1);
    expect(init.image).toBe('public.ecr.aws/amazonlinux/amazonlinux:2023');
    expect(init.essential).toBe(false);
    expect(init.command).toEqual(['chown 10001:10001 /tmp']);
  });

  test('appspec names the app container and port 8080', () => {
    const yaml = appspecYaml();
    expect(yaml).toContain('ContainerName: "email-mcp"');
    expect(yaml).toContain('ContainerPort: 8080');
  });

  test('prepare-taskdef for notify substitutes image and sets -transport notify', () => {
    const liveNotify = {
      taskDefinition: {
        ...liveDescribe.taskDefinition,
        family: 'email-mcp-notify',
        containerDefinitions: [
          {
            name: 'email-mcp-notify',
            image: '222222222222.dkr.ecr.us-east-1.amazonaws.com/email-mcp@sha256:aaa',
            essential: true,
            entryPoint: ['sh', '-c'],
            command: ['mkdir -p /tmp/www && httpd -f -p 8080 -h /tmp/www'],
            environment: [{ name: 'EMAILMCP_TRANSPORT', value: 'notify' }],
          },
          {
            name: 'volume-init',
            image: 'public.ecr.aws/docker/library/busybox:1.36',
            essential: false,
            command: ['chown 10001:10001 /tmp'],
          },
        ],
      },
    };
    const td = prepareTaskdef(liveNotify, { family: 'email-mcp-notify' });
    const app = td.containerDefinitions.find((c: { name: string }) => c.name === 'email-mcp-notify');
    const init = td.containerDefinitions.find((c: { name: string }) => c.name === 'volume-init');
    expect(app.image).toBe('<IMAGE1_NAME>');
    expect(app.entryPoint).toBeUndefined();
    expect(app.command).toEqual(['-transport', 'notify']);
    expect(init.image).toBe('public.ecr.aws/docker/library/busybox:1.36');
    expect(appspecYaml('email-mcp-notify')).toContain('ContainerName: "email-mcp-notify"');
  });
});
