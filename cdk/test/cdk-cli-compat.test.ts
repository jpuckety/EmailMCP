test('aws-cdk-lib stays on cloud assembly schema 54 until CLI 2.1146.1 is published', () => {
  const lib = require('aws-cdk-lib/package.json') as {
    version: string;
    dependencies?: Record<string, string>;
  };
  const schema = lib.dependencies?.['@aws-cdk/cloud-assembly-schema'] ?? '';
  expect(schema).toMatch(/\b54\./);
});
