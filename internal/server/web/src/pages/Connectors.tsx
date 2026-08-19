import {Cable, Play, Trash2} from 'lucide-react';
import {useCallback, useEffect, useState} from 'react';
import {api, type Connector} from '../api';
import {formatDate, relativeTime} from '../format';
import {toast} from '../toast';
import {Empty, PageHead, Status} from '../ui';

export default function Connectors({tenant}: {tenant: string}) {
  const [connectors, setConnectors] = useState<Connector[]>([]);
  const [busy, setBusy] = useState('');

  const load = useCallback(async () => {
    try { setConnectors((await api.listConnectors(tenant)).connectors); }
    catch (error) { toast('Connectors could not load', (error as Error).message, 'error'); }
  }, [tenant]);
  useEffect(() => { void load(); }, [load]);

  const create = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    const slug = String(values.get('slug'));
    setBusy('create');
    try {
      await api.createConnector(tenant, {
        slug,
        dataset: String(values.get('dataset') || slug),
        url: String(values.get('url')),
        records_pointer: String(values.get('records_pointer')),
        refresh_minutes: Number(values.get('refresh_minutes')),
        bearer_token: String(values.get('bearer_token')) || undefined,
      });
      form.reset();
      toast('Connector created', `${slug} will refresh on its configured interval.`);
      await load();
    } catch (error) {
      toast('Connector could not be created', (error as Error).message, 'error');
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
    try { await api.deleteConnector(tenant, connector.slug); await load(); toast('Connector deleted'); }
    catch (error) { toast('Connector could not be deleted', (error as Error).message, 'error'); }
    finally { setBusy(''); }
  };

  return (
    <>
      <PageHead title="Connectors" />
      <form className="connector-form" onSubmit={create}>
        <label>Name<input name="slug" required pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" placeholder="orders-api" /></label>
        <label>Dataset<input name="dataset" pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" placeholder="defaults to connector name" /></label>
        <label className="wide">HTTPS URL<input name="url" type="url" required placeholder="https://api.example.com/orders" /></label>
        <label>Records JSON Pointer<input name="records_pointer" placeholder="/data/items" /></label>
        <label>Refresh minutes<input name="refresh_minutes" type="number" min="1" max="10080" defaultValue="60" required /></label>
        <label className="wide">Bearer token <span className="fine">optional</span><input name="bearer_token" type="password" autoComplete="off" /></label>
        <button className="button" type="submit" disabled={busy === 'create'}><Cable size={14} /> {busy === 'create' ? 'Creating…' : 'Create connector'}</button>
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
            <td className="actions-col"><span className="row-actions"><button className="button small" type="button" disabled={busy === connector.id} onClick={() => sync(connector)}><Play size={13} /> Sync</button><button className="icon-button" type="button" disabled={busy === connector.id} onClick={() => remove(connector)} aria-label={`Delete ${connector.slug}`}><Trash2 size={13} /></button></span></td>
          </tr>)}</tbody>
        </table></div> : <Empty icon={Cable} title="No connectors yet"><p>Create one above, or keep using direct CSV and Parquet uploads.</p></Empty>}
      </section>
    </>
  );
}
