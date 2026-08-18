import {Ban, Clock} from 'lucide-react';
import {useCallback, useEffect, useState} from 'react';
import {api, type Job, type JobLog} from '../api';
import {formatDate, relativeTime, shortId} from '../format';
import {toast} from '../toast';
import {Empty, PageHead, Status} from '../ui';

const active = (job: Job) => ['awaiting_upload', 'queued', 'running'].includes(job.status);

export default function Jobs({tenant}: {tenant: string}) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [selected, setSelected] = useState<Job | null>(null);
  const [logs, setLogs] = useState<JobLog[]>([]);

  const load = useCallback(async () => {
    try {
      const next = (await api.listJobs(tenant)).jobs;
      setJobs(next);
      setSelected(current => current ? next.find(job => job.id === current.id) || null : null);
    } catch (error) {
      toast('Jobs could not load', (error as Error).message, 'error');
    }
  }, [tenant]);

  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    if (!jobs.some(active)) return;
    const timer = window.setTimeout(load, 2000);
    return () => window.clearTimeout(timer);
  }, [jobs, load]);

  const inspect = async (job: Job) => {
    setSelected(job);
    try {
      setLogs((await api.jobLogs(tenant, job.id)).logs);
    } catch (error) {
      toast('Job logs could not load', (error as Error).message, 'error');
    }
  };

  const cancel = async (job: Job) => {
    if (!window.confirm(`Cancel ${job.kind.replaceAll('_', ' ')} ${shortId(job.id)}?`)) return;
    try {
      await api.cancelJob(tenant, job.id);
      toast('Cancellation requested', 'Published data and releases are unchanged.');
      await load();
    } catch (error) {
      toast('Job could not be cancelled', (error as Error).message, 'error');
    }
  };

  return (
    <>
      <PageHead kicker="Background operations" title="Jobs" lede="Inspect imports, connector syncs, and app publishes from one operational ledger." />
      {jobs.length ? (
        <div className="table-wrap">
          <table>
            <thead><tr><th>Job</th><th>Resource</th><th>Status</th><th>Attempts</th><th>Updated</th><th className="actions-col">Actions</th></tr></thead>
            <tbody>{jobs.map(job => (
              <tr key={job.id}>
                <td><button className="table-link" type="button" onClick={() => inspect(job)}>{job.kind.replaceAll('_', ' ')} <code className="id">{shortId(job.id)}</code></button></td>
                <td>{job.dataset_slug || job.app_slug || '—'}</td>
                <td><Status value={job.status} /></td>
                <td className="numeric">{job.attempts}</td>
                <td title={formatDate(job.updated_at)}>{relativeTime(job.updated_at)}</td>
                <td className="actions-col">{active(job) && <button className="button small ghost" type="button" onClick={() => cancel(job)}><Ban size={13} /> Cancel</button>}</td>
              </tr>
            ))}</tbody>
          </table>
        </div>
      ) : <Empty icon={Clock} title="No jobs yet"><p>Uploads, connector syncs, and deployments appear here.</p></Empty>}
      {selected && (
        <section>
          <div className="section-head"><h2>Job {shortId(selected.id)}</h2><Status value={selected.status} /></div>
          {selected.error && <p className="problem" role="alert">{selected.error}</p>}
          <div className="log-view" aria-label="Job logs">
            {logs.length ? logs.map(log => <div key={log.sequence}><time>{formatDate(log.created_at)}</time><strong>{log.level}</strong><span>{log.message}</span></div>) : <p className="fine">No log entries.</p>}
          </div>
        </section>
      )}
    </>
  );
}
