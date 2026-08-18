import {KeyRound, Link2, Trash2, Users} from 'lucide-react';
import {useCallback, useEffect, useState} from 'react';
import {api, type ApiToken, type Invitation, type Member, type Tenant} from '../api';
import {formatDate, relativeTime, shortId} from '../format';
import {toast} from '../toast';
import {Empty, PageHead} from '../ui';

const roles: Member['role'][] = ['owner', 'admin', 'developer', 'viewer'];
const defaultScopes = ['datasets:read', 'queries:read', 'queries:run', 'apps:read', 'jobs:read'];

export default function Access({tenant}: {tenant: Tenant}) {
  const [members, setMembers] = useState<Member[]>([]);
  const [invitations, setInvitations] = useState<Invitation[]>([]);
  const [tokens, setTokens] = useState<ApiToken[]>([]);
  const [secret, setSecret] = useState('');
  const [invitationLink, setInvitationLink] = useState('');
  const canManage = tenant.role === 'owner' || tenant.role === 'admin';

  const load = useCallback(async () => {
    try {
      const [memberResult, tokenResult, invitationResult] = await Promise.all([
        api.listMembers(tenant.slug),
        api.listTokens(tenant.slug),
        canManage ? api.listInvitations(tenant.slug) : Promise.resolve({invitations: []}),
      ]);
      setMembers(memberResult.members);
      setTokens(tokenResult.tokens);
      setInvitations(invitationResult.invitations);
    } catch (error) { toast('Access settings could not load', (error as Error).message, 'error'); }
  }, [canManage, tenant.slug]);
  useEffect(() => { void load(); }, [load]);

  const add = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    try {
      const result = await api.addMember(tenant.slug, String(values.get('email')), String(values.get('role')) as Member['role']);
      form.reset();
      if (result.outcome === 'member_added') {
        setInvitationLink('');
        toast('Member added');
      } else {
        setInvitationLink(result.accept_url);
        toast('Invitation created', 'Copy the link and send it to the invited member.');
      }
      await load();
    } catch (error) { toast('Member could not be added', (error as Error).message, 'error'); }
  };

  const renewInvitation = async (invitation: Invitation) => {
    try {
      const result = await api.renewInvitation(tenant.slug, invitation.id);
      setInvitationLink(result.accept_url);
      await load();
      toast('Invitation renewed', 'The previous link no longer works.');
    } catch (error) { toast('Invitation could not be renewed', (error as Error).message, 'error'); }
  };

  const revokeInvitation = async (invitation: Invitation) => {
    if (!window.confirm(`Revoke the invitation for ${invitation.email}?`)) return;
    try {
      await api.revokeInvitation(tenant.slug, invitation.id);
      setInvitationLink('');
      await load();
      toast('Invitation revoked');
    } catch (error) { toast('Invitation could not be revoked', (error as Error).message, 'error'); }
  };

  const changeRole = async (member: Member, role: Member['role']) => {
    try { await api.changeMemberRole(tenant.slug, member.user_id, role); await load(); }
    catch (error) { toast('Role could not be changed', (error as Error).message, 'error'); }
  };

  const remove = async (member: Member) => {
    if (!window.confirm(`Remove ${member.email} from ${tenant.slug}?`)) return;
    try { await api.removeMember(tenant.slug, member.user_id); await load(); }
    catch (error) { toast('Member could not be removed', (error as Error).message, 'error'); }
  };

  const createToken = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    const scopes = String(values.get('scopes')).split(',').map(value => value.trim()).filter(Boolean);
    try {
      const result = await api.createToken(tenant.slug, String(values.get('name')), scopes, Number(values.get('days')));
      setSecret(result.secret); form.reset(); await load(); toast('Token created', 'Copy the secret now.');
    } catch (error) { toast('Token could not be created', (error as Error).message, 'error'); }
  };

  const revoke = async (token: ApiToken) => {
    if (!window.confirm(`Revoke token ${token.name}?`)) return;
    try { await api.revokeToken(tenant.slug, token.id); await load(); }
    catch (error) { toast('Token could not be revoked', (error as Error).message, 'error'); }
  };

  return (
    <>
      <PageHead kicker="Tenant boundary" title="Access" lede="Manage roles and narrow, expiring API credentials without exposing browser sessions." />
      {canManage && <form className="inline-form" onSubmit={add}><label>Email<input name="email" type="email" required placeholder="teammate@example.com" /></label><label>Role<select name="role" defaultValue="developer">{roles.map(role => <option key={role}>{role}</option>)}</select></label><button className="button" type="submit"><Users size={14} /> Add member</button></form>}
      {invitationLink && <div className="secret-once" role="status"><strong>Copy this invitation link</strong><code>{invitationLink}</code><button className="button small ghost" type="button" onClick={() => { void navigator.clipboard.writeText(invitationLink); toast('Invitation link copied'); }}>Copy</button></div>}
      {canManage && <section>
        <div className="section-head"><h2>Pending invitations</h2><span className="fine">Links expire after 7 days</span></div>
        {invitations.length ? <div className="table-wrap"><table><thead><tr><th>Email</th><th>Role</th><th>Status</th><th>Expires</th><th className="actions-col">Actions</th></tr></thead><tbody>{invitations.map(invitation => <tr key={invitation.id}><td><span className="name">{invitation.email}</span><br /><span className="fine">{shortId(invitation.id)}</span></td><td>{invitation.role}</td><td>{invitation.status}</td><td title={formatDate(invitation.expires_at)}>{relativeTime(invitation.expires_at)}</td><td className="actions-col"><button className="button small ghost" type="button" onClick={() => renewInvitation(invitation)}><Link2 size={13} /> Renew link</button> <button className="icon-button" type="button" onClick={() => revokeInvitation(invitation)} aria-label={`Revoke invitation for ${invitation.email}`}><Trash2 size={13} /></button></td></tr>)}</tbody></table></div> : <p className="fine">No pending or expired invitations.</p>}
      </section>}
      <section>
        <div className="section-head"><h2>Members</h2><span className="fine">{members.length} total</span></div>
        {members.length ? <div className="table-wrap"><table><thead><tr><th>Member</th><th>Role</th><th>Joined</th><th className="actions-col">Actions</th></tr></thead><tbody>{members.map(member => <tr key={member.user_id}><td><span className="name">{member.display_name || member.email}</span><br /><span className="fine">{member.email} · {shortId(member.user_id)}</span></td><td>{canManage ? <select aria-label={`Role for ${member.email}`} value={member.role} onChange={event => changeRole(member, event.target.value as Member['role'])}>{roles.map(role => <option key={role}>{role}</option>)}</select> : member.role}</td><td title={formatDate(member.created_at)}>{relativeTime(member.created_at)}</td><td className="actions-col">{canManage && <button className="icon-button" type="button" onClick={() => remove(member)} aria-label={`Remove ${member.email}`}><Trash2 size={13} /></button>}</td></tr>)}</tbody></table></div> : <Empty icon={Users} title="No members"><p>Every tenant must retain at least one owner.</p></Empty>}
      </section>
      <section>
        <div className="section-head"><h2>API tokens</h2><span className="fine">Only your tokens are shown</span></div>
        <form className="token-form" onSubmit={createToken}><label>Name<input name="name" required placeholder="reporting-script" /></label><label>Expires in days<input name="days" type="number" min="1" max="365" defaultValue="30" required /></label><label className="wide">Scopes, comma separated<input name="scopes" defaultValue={defaultScopes.join(', ')} required /></label><button className="button" type="submit"><KeyRound size={14} /> Create token</button></form>
        {secret && <div className="secret-once" role="status"><strong>Copy this secret now</strong><code>{secret}</code><button className="button small ghost" type="button" onClick={() => { void navigator.clipboard.writeText(secret); toast('Token copied'); }}>Copy</button></div>}
        {tokens.length > 0 && <div className="table-wrap"><table><thead><tr><th>Token</th><th>Scopes</th><th>Expires</th><th>Last used</th><th className="actions-col">Actions</th></tr></thead><tbody>{tokens.map(token => <tr key={token.id}><td><span className="name">{token.name}</span> <code className="id">{shortId(token.id)}</code></td><td className="scope-list">{token.scopes.join(', ')}</td><td title={formatDate(token.expires_at)}>{relativeTime(token.expires_at)}</td><td>{token.last_used_at ? relativeTime(token.last_used_at) : 'never'}</td><td className="actions-col">{token.revoked_at ? <span className="fine">revoked</span> : <button className="button small ghost" type="button" onClick={() => revoke(token)}>Revoke</button>}</td></tr>)}</tbody></table></div>}
      </section>
    </>
  );
}
