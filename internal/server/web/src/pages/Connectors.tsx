import {Cable, Pencil, Play, Trash2, X} from 'lucide-react';
import {useCallback, useEffect, useState} from 'react';
import {api, type Connector} from '../api';
import {formatDate, relativeTime} from '../format';
import {toast} from '../toast';
import {Empty, PageHead, Status} from '../ui';

export default function Connectors({tenant}: {tenant: string}) {
  const [connectors, setConnectors] = useState<Connector[]>([]);
  const [busy, setBusy] = useState('');
  const [editing, setEditing] = useState<Connector | null>(null);

  const load = useCallback(async () => {
    try { setConnectors((await api.listConnectors(tenant)).connectors); }
    catch (error) { toast('Connectors could not load', (error as Error).message, 'error'); }
  }, [tenant]);
  useEffect(() => { void load(); }, [load]);

  const save = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    const slug = editing?.slug || String(values.get('slug'));
    const bearerToken = String(values.get('bearer_token'));
    const clearBearer = values.get('clear_bearer') === 'on';
    const input = {
      slug,
      dataset: editing?.dataset_slug || String(values.get('dataset') || slug),
      url: String(values.get('url')),
      records_pointer: String(values.get('records_pointer')),
      cursor_parameter: String(values.get('cursor_parameter')) || undefined,
      next_cursor_pointer: String(values.get('next_cursor_pointer')) || undefined,
      refresh_minutes: Number(values.get('refresh_minutes')),
      enabled: values.get('enabled') === 'true',
      bearer_token: bearerToken || clearBearer ? bearerToken : undefined,
    };
    setBusy(editing?.id || 'create');
    try {
      if (editing) await api.updateConnector(tenant, slug, input);
      else await api.createConnector(tenant, input);
      form.reset();
      setEditing(null);
      toast(editing ? 'Connector updated' : 'Connector created', `${slug} will refresh on its configured interval.`);
      await load();
    } catch (error) {
      toast(`Connector could not be ${editing ? 'updated' : 'created'}`, (error as Error).message, 'error');
    } finally { setBusy(''); }
  };

  const sync = async (connector: Connector) => {
    setBusy(connector.id);
    try {
      await api.syncConnector(tenant, connector.slug);
      toast('Sync queued', `${connector.slug} will publish atomically when it succeeds.`);
      await load();
    } catch (error) { toast('Sync could not be queued', (error as Error).message, 'error'); }
    finally { setBusy(''); }
  };

  const remove = async (connector: Connector) => {
    if (!window.confirm(`Delete connector ${connector.slug}? Its dataset and published snapshot remain.`)) return;
    setBusy(connector.id);
    try { await api.deleteConnector(tenant, connector.slug); if (editing?.id === connector.id) setEditing(null); await load(); toast('Connector deleted'); }
    catch (error) { toast('Connector could not be deleted', (error as Error).message, 'error'); }
    finally { setBusy(''); }
  };

  return (
    <>
      <PageHead title="Connectors" />
      <form key={editing?.id || 'create'} className="connector-form" onSubmit={save}>
        <label>Name<input name="slug" required readOnly={!!editing} defaultValue={editing?.slug} pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" placeholder="orders-api" /></label>
        <label>Dataset<input name="dataset" readOnly={!!editing} defaultValue={editing?.dataset_slug} pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" placeholder="defaults to connector name" /></label>
        <label className="wide">HTTPS URL<input name="url" type="url" required defaultValue={editing?.url} placeholder="https://api.example.com/orders" /></label>
        <label>Records JSON Pointer<input name="records_pointer" defaultValue={editing?.records_pointer} placeholder="/data/items" /></label>
        <label>Refresh minutes<input name="refresh_minutes" type="number" min="1" max="10080" defaultValue={editing?.refresh_minutes || 60} required /></label>
        <label>Cursor query parameter <span className="fine">optional</span><input name="cursor_parameter" defaultValue={editing?.cursor_parameter} placeholder="cursor" /></label>
        <label>Next cursor JSON Pointer <span className="fine">optional</span><input name="next_cursor_pointer" defaultValue={editing?.next_cursor_pointer} placeholder="/meta/next" /></label>
        <label>Status<select name="enabled" defaultValue={String(editing?.enabled ?? true)}><option value="true">Enabled</option><option value="false">Disabled</option></select></label>
        <label className="wide">Bearer token <span className="fine">{editing?.auth_configured ? 'leave blank to keep current token' : 'optional'}</span><input name="bearer_token" type="password" autoComplete="off" /></label>
        {editing?.auth_configured && <label className="check wide"><input name="clear_bearer" type="checkbox" /> Remove current bearer token</label>}
        <span className="row-actions">
          <button className="button" type="submit" disabled={!!busy}>
            {editing ? <Pencil size={14} /> : <Cable size={14} />} {busy ? 'Saving…' : editing ? 'Save connector' : 'Create connector'}
          </button>
          {editing && <button className="button ghost" type="button" disabled={!!busy} onClick={() => setEditing(null)}><X size={14} /> Cancel</button>}
        </span>
      </form>
      <section>
        <div className="section-head"><h2>Configured connectors</h2><span className="fine">{connectors.length} total</span></div>
        {connectors.length ? <div className="table-wrap"><table>
          <thead><tr><th>Connector</th><th>Dataset</th><th>Schedule</th><th>Last sync</th><th>Next sync</th><th className="actions-col">Actions</th></tr></thead>
          <tbody>{connectors.map(connector => <tr key={connector.id}>
            <td><span className="name">{connector.slug}</span><br /><span className="fine">{connector.auth_configured ? 'bearer auth' : 'no auth'}</span></td>
            <td>{connector.dataset_slug}</td>
            <td>{connector.enabled ? `every ${connector.refresh_minutes} min` : 'disabled'}</td>
            <td>{connector.last_status ? <><Status value={connector.last_status} /> <span className="fine">{connector.last_finished_at && relativeTime(connector.last_finished_at)}</span></> : '—'}</td>
            <td title={formatDate(connector.next_sync_at)}>{relativeTime(connector.next_sync_at)}</td>
            <td className="actions-col"><span className="row-actions"><button className="button small" type="button" disabled={!!busy} onClick={() => sync(connector)}><Play size={13} /> Sync</button><button className="icon-button" type="button" disabled={!!busy} onClick={() => setEditing(connector)} aria-label={`Edit ${connector.slug}`}><Pencil size={13} /></button><button className="icon-button" type="button" disabled={!!busy} onClick={() => remove(connector)} aria-label={`Delete ${connector.slug}`}><Trash2 size={13} /></button></span></td>
          </tr>)}</tbody>
        </table></div> : <Empty icon={Cable} title="No connectors yet"><p>Create one above, or keep using direct CSV and Parquet uploads.</p></Empty>}
      </section>
    </>
  );
}
