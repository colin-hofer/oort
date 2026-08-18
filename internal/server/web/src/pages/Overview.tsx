import {ArrowRight, Clock} from 'lucide-react';
import type {Dashboard} from '../api';
import {formatBytes, formatDate, formatNumber, relativeTime} from '../format';
import {Command, Empty, PageHead, Status} from '../ui';

// The overview leads with the next useful action, not vanity metrics.
function nextStep(dashboard: Dashboard) {
  const tenant = dashboard.tenant.slug;
  if (!dashboard.datasets.length) {
    return {
      title: 'Upload your first dataset',
      copy: 'CSV and Parquet files become tenant-isolated snapshots. A failed import never replaces the last good version.',
      href: '#/datasets',
      label: 'Go to datasets',
      command: `neb dataset upload customers.csv --tenant ${tenant}`,
    };
  }
  if (!dashboard.queries.length) {
    return {
      title: 'Save a query',
      copy: 'Write the exact read path your app will call, bind its parameters, and inspect the real result.',
      href: '#/queries',
      label: 'Open the workbench',
      command: `neb query run queries/customers.sql --tenant ${tenant}`,
    };
  }
  const live = dashboard.apps.find(app => app.current_deployment_id);
  if (!live) {
    return {
      title: 'Deploy an app',
      copy: 'Bundle the static frontend and pin the query revisions it may call. Releases are immutable; rollback moves one pointer.',
      href: '#/apps',
      label: 'Review apps',
      command: `neb app deploy --tenant ${tenant}`,
    };
  }
  return {
    title: `${live.slug} is live`,
    copy: 'Open the current release as a tenant member and keep the previous release one rollback away.',
    href: '#/apps',
    label: 'Open apps',
    command: `neb app open --tenant ${tenant}`,
  };
}

export function SyncsTable({dashboard, limit}: {dashboard: Dashboard; limit?: number}) {
  const syncs = limit ? dashboard.syncs.slice(0, limit) : dashboard.syncs;
  if (!syncs.length) {
    return (
      <Empty icon={Clock} title="No imports yet">
        <p>Dataset uploads appear here with status, rows, and timing.</p>
      </Empty>
    );
  }
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr><th>Dataset</th><th>Status</th><th>Rows</th><th>Size</th><th>Started</th></tr>
        </thead>
        <tbody>
          {syncs.map(sync => (
            <tr key={sync.id}>
              <td className="name">{sync.dataset_slug}</td>
              <td><Status value={sync.status} /></td>
              <td className="numeric">{sync.row_count == null ? '—' : formatNumber(sync.row_count)}</td>
              <td className="numeric">{formatBytes(sync.byte_count)}</td>
              <td title={formatDate(sync.created_at)}>{relativeTime(sync.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default function Overview({dashboard}: {dashboard: Dashboard}) {
  const step = nextStep(dashboard);
  const failures = [...dashboard.syncs, ...dashboard.deployments].filter(item => item.status === 'failed').length;
  const facts: [string, string][] = [
    ['Datasets', String(dashboard.datasets.length)],
    ['Queries', String(dashboard.queries.length)],
    ['Apps', String(dashboard.apps.length)],
    ['Failures', failures ? String(failures) : 'none'],
  ];
  return (
    <>
      <PageHead kicker={`${dashboard.tenant.slug} · local`} title="Overview" />
      <dl className="facts">
        {facts.map(([label, value]) => (
          <div key={label} className={label === 'Failures' && failures ? 'fact fact-bad' : 'fact'}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <section className="next-step">
        <p className="kicker">Next useful action</p>
        <h2>{step.title}</h2>
        <p>{step.copy}</p>
        <div className="next-step-actions">
          <a className="button" href={step.href}>{step.label} <ArrowRight size={14} aria-hidden="true" /></a>
          <Command text={step.command} />
        </div>
      </section>
      <section>
        <div className="section-head">
          <h2>Recent imports</h2>
          <a className="text-link" href="#/activity">All activity</a>
        </div>
        <SyncsTable dashboard={dashboard} limit={5} />
      </section>
    </>
  );
}
