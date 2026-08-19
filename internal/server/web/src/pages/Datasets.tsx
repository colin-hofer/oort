import {Database, Upload} from 'lucide-react';
import {useState} from 'react';
import {api, type Dashboard} from '../api';
import {formatBytes, formatNumber, relativeTime, shortId} from '../format';
import {toast} from '../toast';
import {Empty, PageHead, Status} from '../ui';
import {SyncsTable} from './Overview';

const slugFromFilename = (name: string) =>
  name.replace(/\.(csv|parquet)$/i, '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 63);

function UploadForm({tenant, onDone}: {tenant: string; onDone: () => Promise<void>}) {
  const [slug, setSlug] = useState('');
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
        <Upload size={14} aria-hidden="true" /> {busy ? 'Uploading…' : 'Upload'}
      </button>
    </form>
  );
}

export default function Datasets({dashboard, reload}: {dashboard: Dashboard; reload: () => Promise<void>}) {
  const {datasets, syncs, tenant} = dashboard;
  return (
    <>
      <PageHead title="Datasets" />
      <section>
        <div className="section-head">
          <h2>Upload</h2>
          <span className="fine">CSV or Parquet, up to 1 GB. The name becomes the SQL table.</span>
        </div>
        <UploadForm tenant={tenant.slug} onDone={reload} />
      </section>
      {datasets.length ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr><th>Dataset</th><th>Columns</th><th>Rows</th><th>Stored</th><th>Snapshot</th><th>Updated</th></tr>
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
