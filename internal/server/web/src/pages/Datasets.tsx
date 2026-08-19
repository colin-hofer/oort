import {Database, Trash2, Upload} from 'lucide-react';
import {useState} from 'react';
import {api, type Dashboard} from '../api';
import {formatBytes, formatNumber, relativeTime, shortId} from '../format';
import {toast} from '../toast';
import {Empty, PageHead, Status} from '../ui';
import {SyncsTable} from './Overview';

const slugFromFilename = (name: string) =>
  name.replace(/\.(csv|parquet)$/i, '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 63);

function UploadForm({tenant, initialSlug = '', onDone}: {tenant: string; initialSlug?: string; onDone: () => Promise<void>}) {
  const [slug, setSlug] = useState(initialSlug);
  const [busy, setBusy] = useState(false);

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const file = new FormData(event.currentTarget).get('file');
    if (!(file instanceof File) || !file.size) return;
    setBusy(true);
    try {
      await api.uploadDataset(tenant, slug, file);
      toast('Upload queued', `${slug} is being imported.`);
      setSlug('');
      await onDone();
    } catch (error) {
      toast('Upload failed', (error as Error).message, 'error');
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="inline-form" onSubmit={submit}>
      <label>
        Name
        <input
          name="slug"
          value={slug}
          onChange={event => setSlug(event.target.value)}
          readOnly={!!initialSlug}
          required
          minLength={3}
          maxLength={63}
          pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]"
          placeholder="customers"
          autoComplete="off"
        />
      </label>
      <label>
        File
        <input
          name="file"
          type="file"
          accept=".csv,.parquet"
          required
          onChange={event => {
            const file = event.target.files?.[0];
            if (file && !slug) setSlug(slugFromFilename(file.name));
          }}
        />
      </label>
      <button className="button" type="submit" disabled={busy}>
        <Upload size={14} aria-hidden="true" /> {busy ? 'Uploading…' : initialSlug ? 'Replace' : 'Upload'}
      </button>
    </form>
  );
}

export default function Datasets({dashboard, reload}: {dashboard: Dashboard; reload: () => Promise<void>}) {
  const {datasets, syncs, tenant} = dashboard;
  const [replacing, setReplacing] = useState('');
  const [deleting, setDeleting] = useState('');

  const remove = async (slug: string) => {
    if (!window.confirm(`Delete dataset ${slug}? Its snapshots, import history, and connector will be removed.`)) return;
    setDeleting(slug);
    try {
      await api.deleteDataset(tenant.slug, slug);
      if (replacing === slug) setReplacing('');
      await reload();
      toast('Dataset deleted', slug);
    } catch (error) {
      toast('Dataset could not be deleted', (error as Error).message, 'error');
    } finally {
      setDeleting('');
    }
  };

  return (
    <>
      <PageHead title="Datasets" />
      <section>
        <div className="section-head">
          <h2>{replacing ? `Replace ${replacing}` : 'Upload'}</h2>
          {replacing
            ? <button className="button small ghost" type="button" onClick={() => setReplacing('')}>Cancel</button>
            : <span className="fine">CSV or Parquet, up to 1 GB. The name becomes the SQL table.</span>}
        </div>
        <UploadForm
          key={replacing || 'upload'}
          tenant={tenant.slug}
          initialSlug={replacing}
          onDone={async () => { setReplacing(''); await reload(); }}
        />
      </section>
      {datasets.length ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr><th>Dataset</th><th>Columns</th><th>Rows</th><th>Stored</th><th>Snapshot</th><th>Updated</th><th className="actions-col">Actions</th></tr>
            </thead>
            <tbody>
              {datasets.map(dataset => (
                <tr key={dataset.id}>
                  <td className="name">
                    {dataset.slug} <code className="id">{shortId(dataset.id)}</code>
                  </td>
                  <td className="numeric">{Array.isArray(dataset.schema) ? dataset.schema.length : '—'}</td>
                  <td className="numeric">{dataset.row_count == null ? '—' : formatNumber(dataset.row_count)}</td>
                  <td className="numeric">{formatBytes(dataset.byte_count)}</td>
                  <td>
                    {dataset.current_snapshot_id == null
                      ? <Status value={dataset.last_sync_status || 'queued'} />
                      : <code className="id">#{dataset.current_snapshot_id}</code>}
                  </td>
                  <td>{relativeTime(dataset.updated_at)}</td>
                  <td className="actions-col">
                    <span className="row-actions">
                      <button className="button small ghost" type="button" onClick={() => setReplacing(dataset.slug)}>
                        <Upload size={13} aria-hidden="true" /> Replace
                      </button>
                      <button className="icon-button" type="button" disabled={deleting === dataset.slug} onClick={() => remove(dataset.slug)} aria-label={`Delete ${dataset.slug}`}>
                        <Trash2 size={13} aria-hidden="true" />
                      </button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={Database} title="No datasets yet">
          <p>Upload a CSV or Parquet file. Oort infers its schema and publishes one atomic snapshot.</p>
        </Empty>
      )}
      {syncs.length > 0 && (
        <section>
          <div className="section-head">
            <h2>Import history</h2>
            <span className="fine">Latest {syncs.length} syncs</span>
          </div>
          <SyncsTable dashboard={dashboard} />
        </section>
      )}
    </>
  );
}
