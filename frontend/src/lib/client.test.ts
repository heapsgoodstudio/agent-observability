import { createRouterTransport } from '@connectrpc/connect';
import { describe, expect, it } from 'vitest';
import { ObsService } from '../gen/claudeobs/v1/obs_pb';
import { createObsClient } from './client';

// The RPCs the whole frontend calls through obsClient (proto/claudeobs/v1/obs.proto's
// ObsService). Listed explicitly so a proto edit that drops or renames one fails this test
// rather than failing silently at the call site.
const RPC_METHOD_NAMES = [
  'getSettings',
  'saveProfile',
  'removeProfile',
  'setAccountHidden',
  'repairProfile',
  'importHistory',
  'getMonitoring',
  'getPayAsYouGo',
  'getMeta',
  'getAnalysisModels',
  'getOverview',
  'getSpend',
  'getFindings',
  'getHealth',
  'listProfiles',
  'getUsageSnapshot',
  'getAccountHistory',
  'listSessions',
  'getSession',
  'startAnalysis',
  'getAnalysis',
  'startAdvice',
  'getAdvice',
];

describe('ObsService', () => {
  it('exposes the dashboard and profile management RPC methods', () => {
    expect(Object.keys(ObsService.method)).toEqual(RPC_METHOD_NAMES);
  });
});

describe('createObsClient', () => {
  it('round-trips a GetMetaResponse through a mocked transport', async () => {
    const transport = createRouterTransport(({ service }) => {
      service(ObsService, {
        getMeta: () => ({
          identity: {
            machineId: 'test-machine',
            hostname: 'test-host',
            user: 'alice',
            generatedAt: '2026-08-16T00:00:00Z',
          },
          version: 'v2.0.0-test',
          installDir: '/opt/claude-obs',
          dashPort: 8427,
          vmPort: 8428,
          vlPort: 9428,
          analysisTimeoutS: 1500,
          adviceEvidenceTimeoutS: 2700,
          analysisModels: ['sonnet'],
          adviceDepths: ['shallow', 'standard', 'evidence'],
        }),
      });
    });

    const client = createObsClient(transport);
    const response = await client.getMeta({});

    expect(response.version).toBe('v2.0.0-test');
    expect(response.identity?.hostname).toBe('test-host');
    expect(response.dashPort).toBe(8427);
    expect(response.adviceDepths).toEqual(['shallow', 'standard', 'evidence']);
  });
});
