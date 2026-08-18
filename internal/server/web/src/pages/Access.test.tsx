// @vitest-environment jsdom

import {cleanup, fireEvent, render, screen, waitFor} from '@testing-library/react';
import {afterEach, describe, expect, it, vi} from 'vitest';
import Access from './Access';

const mocks = vi.hoisted(() => ({
  listMembers: vi.fn(),
  listTokens: vi.fn(),
  listInvitations: vi.fn(),
  addMember: vi.fn(),
  renewInvitation: vi.fn(),
  revokeInvitation: vi.fn(),
}));

vi.mock('../api', () => ({api: {
  ...mocks,
  changeMemberRole: vi.fn(),
  removeMember: vi.fn(),
  createToken: vi.fn(),
  revokeToken: vi.fn(),
}}));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe('Access membership invitations', () => {
  it('creates, lists, renews, and revokes copyable links', async () => {
    const invitation = {
      id: '11111111-1111-4111-8111-111111111111', tenant: 'acme', email: 'pending@example.com',
      role: 'developer' as const, invited_by_user_id: 'owner-id', status: 'pending' as const,
      created_at: '2026-08-18T00:00:00Z', expires_at: '2026-08-25T00:00:00Z',
    };
    mocks.listMembers.mockResolvedValue({members: []});
    mocks.listTokens.mockResolvedValue({tokens: []});
    mocks.listInvitations.mockResolvedValue({invitations: [invitation]});
    mocks.addMember.mockResolvedValue({outcome: 'invitation_created', invitation, accept_url: 'https://neb.test/auth/invitations/created'});
    mocks.renewInvitation.mockResolvedValue({outcome: 'invitation_renewed', invitation, accept_url: 'https://neb.test/auth/invitations/renewed'});
    mocks.revokeInvitation.mockResolvedValue(undefined);
    vi.stubGlobal('confirm', vi.fn(() => true));

    render(<Access tenant={{id: 'tenant-id', slug: 'acme', role: 'owner', created_at: '2026-08-18T00:00:00Z'}} />);

    expect(await screen.findByText('pending@example.com')).toBeTruthy();
    fireEvent.change(screen.getByRole('textbox', {name: 'Email'}), {target: {value: 'new@example.com'}});
    fireEvent.click(screen.getByRole('button', {name: 'Add member'}));
    expect(await screen.findByText('https://neb.test/auth/invitations/created')).toBeTruthy();
    expect(mocks.addMember).toHaveBeenCalledWith('acme', 'new@example.com', 'developer');

    fireEvent.click(screen.getByRole('button', {name: 'Renew link'}));
    expect(await screen.findByText('https://neb.test/auth/invitations/renewed')).toBeTruthy();
    expect(mocks.renewInvitation).toHaveBeenCalledWith('acme', invitation.id);

    fireEvent.click(screen.getByRole('button', {name: 'Revoke invitation for pending@example.com'}));
    await waitFor(() => expect(mocks.revokeInvitation).toHaveBeenCalledWith('acme', invitation.id));
  });
});
