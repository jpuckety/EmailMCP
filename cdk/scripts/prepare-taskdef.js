'use strict';

/**
 * Turn `aws ecs describe-task-definition` JSON into a CodeDeploy taskdef.json:
 * strip registration-only fields, replace only the app container image with
 * `<IMAGE1_NAME>`, and drop the placeholder entryPoint/command so the real
 * image ENTRYPOINT runs. The volume-init sidecar is left unchanged.
 *
 * Usage: node prepare-taskdef.js [input.json] [family-or-container] > taskdef.json
 */

const APP_CONTAINER_NAME = 'email-mcp';
const IMAGE_PLACEHOLDER = '<IMAGE1_NAME>';
const NOTIFY_COMMAND = ['-transport', 'notify'];

const CODE_DEPLOY_TARGETS = [
  { family: 'email-mcp', containerName: 'email-mcp', containerPort: 8080 },
  {
    family: 'email-mcp-notify',
    containerName: 'email-mcp-notify',
    containerPort: 8080,
    command: NOTIFY_COMMAND,
  },
];

const STRIP_FIELDS = [
  'taskDefinitionArn',
  'revision',
  'status',
  'requiresAttributes',
  'compatibilities',
  'registeredAt',
  'registeredBy',
  'deregisteredAt',
];

function unwrapTaskDefinition(input) {
  if (input && typeof input === 'object' && input.taskDefinition) {
    return input.taskDefinition;
  }
  return input;
}

function stripRegisteredFields(td) {
  const clone = JSON.parse(JSON.stringify(td));
  for (const field of STRIP_FIELDS) {
    delete clone[field];
  }
  return clone;
}

function substituteAppImage(td, placeholder, containerName) {
  const name = containerName || APP_CONTAINER_NAME;
  const ph = placeholder || IMAGE_PLACEHOLDER;
  if (!Array.isArray(td.containerDefinitions)) {
    throw new Error('task definition missing containerDefinitions');
  }
  let found = false;
  for (const container of td.containerDefinitions) {
    if (container.name === name) {
      container.image = ph;
      found = true;
    }
  }
  if (!found) {
    throw new Error(`container '${name}' not found in task definition`);
  }
  return td;
}

function clearAppPlaceholderOverride(td, containerName) {
  const name = containerName || APP_CONTAINER_NAME;
  if (!Array.isArray(td.containerDefinitions)) {
    return td;
  }
  for (const container of td.containerDefinitions) {
    if (container.name === name) {
      delete container.command;
      delete container.entryPoint;
    }
  }
  return td;
}

function setAppCommand(td, containerName, command) {
  const name = containerName || APP_CONTAINER_NAME;
  if (!Array.isArray(td.containerDefinitions)) {
    return td;
  }
  for (const container of td.containerDefinitions) {
    if (container.name === name) {
      delete container.entryPoint;
      container.command = command;
    }
  }
  return td;
}

function targetFor(name) {
  if (!name) {
    return CODE_DEPLOY_TARGETS[0];
  }
  return (
    CODE_DEPLOY_TARGETS.find((target) => target.family === name || target.containerName === name) || {
      family: name,
      containerName: name,
      containerPort: 8080,
    }
  );
}

function prepareTaskdef(input, options) {
  const opts = options || {};
  const target = targetFor(opts.containerName || opts.family);
  const containerName = opts.containerName || target.containerName;
  const td = stripRegisteredFields(unwrapTaskDefinition(input));
  substituteAppImage(td, opts.placeholder, containerName);
  if (opts.command || target.command) {
    return setAppCommand(td, containerName, opts.command || target.command);
  }
  return clearAppPlaceholderOverride(td, containerName);
}

function appspecYaml(containerName, containerPort) {
  const target = targetFor(containerName);
  const name = containerName || target.containerName || APP_CONTAINER_NAME;
  const port = containerPort || target.containerPort || 8080;
  return [
    'version: 0.0',
    'Resources:',
    '  - TargetService:',
    '      Type: AWS::ECS::Service',
    '      Properties:',
    '        TaskDefinition: <TASK_DEFINITION>',
    '        LoadBalancerInfo:',
    `          ContainerName: "${name}"`,
    `          ContainerPort: ${port}`,
    '',
  ].join('\n');
}

module.exports = {
  APP_CONTAINER_NAME,
  IMAGE_PLACEHOLDER,
  CODE_DEPLOY_TARGETS,
  unwrapTaskDefinition,
  stripRegisteredFields,
  substituteAppImage,
  clearAppPlaceholderOverride,
  setAppCommand,
  targetFor,
  prepareTaskdef,
  appspecYaml,
};

if (require.main === module) {
  const fs = require('fs');
  const path = process.argv[2];
  const targetName = process.argv[3];
  const raw = path ? fs.readFileSync(path, 'utf8') : fs.readFileSync(0, 'utf8');
  const opts = targetName ? { family: targetName, containerName: targetFor(targetName).containerName } : {};
  process.stdout.write(JSON.stringify(prepareTaskdef(JSON.parse(raw), opts), null, 2) + '\n');
}
