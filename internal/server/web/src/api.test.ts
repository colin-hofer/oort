import {afterEach, describe, expect, it, vi} from 'vitest';
import {api} from './api';

afterEach(() => vi.unstubAllGlobals());

describe('query API', () => {
  it('keeps draft execution separate from saving a revision', async () => {
    const fetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({result: {columns: [], rows: [], snapshot_id: 1, truncated: false}}), {status: 200}))
      .mockResolvedValueOnce(new Response(JSON.stringify({query: {slug: 'orders', version: 1}}), {status: 200}));
    vi.stubGlobal('fetch', fetch);

    await api.executeDraftQuery('acme', 'SELECT 1', {});
    await api.saveQuery('acme', 'orders', 'SELECT 1', {});

    expect(fetch.mock.calls[0][0]).toBe('/v1/tenants/acme/queries/execute');
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({sql: 'SELECT 1', parameters: {}});
    expect(fetch.mock.calls[1][0]).toBe('/v1/tenants/acme/queries/orders');
    expect(fetch.mock.calls[1][1].method).toBe('PUT');
  });

  it('preserves stable server error details', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({
      error: {code: 'invalid_query', message: 'SELECT only', request_id: 'request-id'},
    }), {status: 422})));

    await expect(api.executeDraftQuery('acme', 'DELETE FROM orders', {})).rejects.toMatchObject({
      status: 422, code: 'invalid_query', requestId: 'request-id', message: 'SELECT only',
    });
  });
});

describe('resource lifecycle API', () => {
  it('uses canonical mutation routes', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(null, {status: 204}));
    vi.stubGlobal('fetch', fetch);

    await api.deleteDataset('acme', 'orders');
    await api.deleteQuery('acme', 'recent-orders');
    await api.deleteConnector('acme', 'orders-api');
    await api.deleteApp('acme', 'sales');

    expect(fetch.mock.calls.map(call => [call[0], call[1].method])).toEqual([
      ['/v1/tenants/acme/datasets/orders', 'DELETE'],
      ['/v1/tenants/acme/queries/recent-orders', 'DELETE'],
      ['/v1/tenants/acme/connectors/orders-api', 'DELETE'],
      ['/v1/tenants/acme/apps/sales', 'DELETE'],
    ]);
  });

  it('updates a connector in place', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({connector: {slug: 'orders-api'}}), {status: 200}));
    vi.stubGlobal('fetch', fetch);

    await api.updateConnector('acme', 'orders-api', {url: 'https://api.example.test/orders', enabled: false});

    expect(fetch.mock.calls[0][0]).toBe('/v1/tenants/acme/connectors/orders-api');
    expect(fetch.mock.calls[0][1].method).toBe('PUT');
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({url: 'https://api.example.test/orders', enabled: false});
  });
});

describe('job API', () => {
  it('uses the canonical jobs resource', async () => {
    const fetch = vi.fn().mockImplementation(() => Promise.resolve(
      new Response(JSON.stringify({jobs: [], job: {id: 'job-id'}, logs: []}), {status: 200}),
    ));
    vi.stubGlobal('fetch', fetch);

    await api.listJobs('acme');
    await api.getJob('acme', 'job-id');
    await api.jobLogs('acme', 'job-id');
    await api.cancelJob('acme', 'job-id');

    expect(fetch.mock.calls.map(call => call[0])).toEqual([
      '/v1/tenants/acme/jobs',
      '/v1/tenants/acme/jobs/job-id',
      '/v1/tenants/acme/jobs/job-id/logs?after=0',
      '/v1/tenants/acme/jobs/job-id/cancel',
    ]);
  });
});

describe('membership invitation API', () => {
  it('uses the nested invitation resources and preserves creation outcomes', async () => {
    const created = {outcome: 'invitation_created', invitation: {id: 'invite-id'}, accept_url: 'https://oort.test/auth/invitations/secret'};
    const fetch = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify(created), {status: 201}))
      .mockResolvedValueOnce(new Response(JSON.stringify({invitations: []}), {status: 200}))
      .mockResolvedValueOnce(new Response(JSON.stringify({...created, outcome: 'invitation_renewed'}), {status: 200}))
      .mockResolvedValueOnce(new Response(null, {status: 204}));
    vi.stubGlobal('fetch', fetch);

    const outcome = await api.addMember('acme', 'new@example.com', 'developer');
    await api.listInvitations('acme');
    await api.renewInvitation('acme', 'invite-id');
    await api.revokeInvitation('acme', 'invite-id');

    expect(outcome).toEqual(created);
    expect(fetch.mock.calls.map(call => [call[0], call[1].method || 'GET'])).toEqual([
      ['/v1/tenants/acme/members', 'POST'],
      ['/v1/tenants/acme/members/invitations', 'GET'],
      ['/v1/tenants/acme/members/invitations/invite-id/renew', 'POST'],
      ['/v1/tenants/acme/members/invitations/invite-id', 'DELETE'],
    ]);
  });
});
