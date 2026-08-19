import {AppWindow, ExternalLink, RotateCcw} from 'lucide-react';
import {useState} from 'react';
import {api, type Dashboard} from '../api';
import {formatBytes, relativeTime, shortId} from '../format';
import {toast} from '../toast';
import {Command, Empty, PageHead, Status} from '../ui';

async function openApp(tenant: string, slug: string) {
  // Open the window synchronously so popup blockers attribute it to the click.
  const opened = window.open('about:blank', '_blank');
  try {
    const {url} = await api.appLoginLink(tenant, slug);
    if (opened) opened.location = url;
    else window.location.href = url;
  } catch (error) {
    opened?.close();
    toast('App could not open', (error as Error).message, 'error');
  }
}

export default function Apps({dashboard, reload}: {dashboard: Dashboard; reload: () => Promise<void>}) {
  const {apps, deployments, tenant} = dashboard;
  const [rollingBack, setRollingBack] = useState('');

  const rollback = async (deployment: string, appSlug: string, version: number) => {
    if (!window.confirm(`Point ${appSlug} back to release v${version}?`)) return;
    setRollingBack(deployment);
    try {
      await api.rollback(tenant.slug, deployment);
      toast('Release restored', `${appSlug} now serves v${version}.`);
      await reload();
    } catch (error) {
      toast('Rollback failed', (error as Error).message, 'error');
    } finally {
      setRollingBack('');
    }
  };

  return (
    <>
      <PageHead title="Apps">
        <Command text={`oort app deploy --tenant ${tenant.slug}`} />
      </PageHead>
      {apps.length ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr><th>App</th><th>Status</th><th>Release</th><th>Updated</th><th className="actions-col">Actions</th></tr>
            </thead>
            <tbody>
              {apps.map(app => (
                <tr key={app.id}>
                  <td className="name">{app.slug} <code className="id">{shortId(app.id)}</code></td>
                  <td><Status value={app.current_status || 'queued'} /></td>
                  <td>{app.current_version ? `v${app.current_version}` : '—'}</td>
                  <td>{relativeTime(app.updated_at)}</td>
                  <td className="actions-col">
                    <button
                      type="button"
                      className="button small"
                      onClick={() => openApp(tenant.slug, app.slug)}
                      disabled={!app.current_deployment_id}
                    >
                      <ExternalLink size={13} aria-hidden="true" /> Open
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={AppWindow} title="No apps yet">
          <p>Deploy from a project containing oort.json. The CLI validates the bundle and prints the exact rollback command.</p>
        </Empty>
      )}
      {deployments.length > 0 && (
        <section>
          <div className="section-head">
            <h2>Release history</h2>
            <span className="fine">Immutable by design</span>
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr><th>Release</th><th>Status</th><th>Size</th><th>When</th><th className="actions-col">Actions</th></tr>
              </thead>
              <tbody>
                {deployments.map(deployment => {
                  const current = apps.some(app => app.current_deployment_id === deployment.id);
                  return (
                    <tr key={deployment.id}>
                      <td className="name">
                        {deployment.app_slug} v{deployment.version} <code className="id">{shortId(deployment.id)}</code>
                      </td>
                      <td><Status value={deployment.status} /></td>
                      <td className="numeric">{formatBytes(deployment.byte_count)}</td>
                      <td>
                        {deployment.published_at
                          ? `published ${relativeTime(deployment.published_at)}`
                          : `created ${relativeTime(deployment.created_at)}`}
                      </td>
                      <td className="actions-col">
                        {current && <span className="fine">current</span>}
                        {!current && deployment.status === 'succeeded' && (
                          <button
                            type="button"
                            className="button small ghost"
                            onClick={() => rollback(deployment.id, deployment.app_slug, deployment.version)}
                            disabled={rollingBack === deployment.id}
                          >
                            <RotateCcw size={13} aria-hidden="true" /> Roll back
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </>
  );
}
