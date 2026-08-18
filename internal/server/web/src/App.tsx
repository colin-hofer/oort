import {Activity, AppWindow, Cable, Clock3, Database, Home, LogOut, SquareTerminal, UsersRound} from 'lucide-react';
import {useCallback, useEffect, useState, useSyncExternalStore} from 'react';
import {api, ApiError, type Dashboard, type Tenant, type User} from './api';
import Connect from './pages/Connect';
import ActivityPage from './pages/Activity';
import Apps from './pages/Apps';
import Datasets from './pages/Datasets';
import Overview from './pages/Overview';
import Queries from './pages/Queries';
import Jobs from './pages/Jobs';
import Connectors from './pages/Connectors';
import Access from './pages/Access';
import {toast} from './toast';
import {Logo, Toasts} from './ui';

const pages = [
  {key: 'overview', label: 'Overview', icon: Home},
  {key: 'datasets', label: 'Datasets', icon: Database},
  {key: 'queries', label: 'Queries', icon: SquareTerminal},
  {key: 'connectors', label: 'Connectors', icon: Cable},
  {key: 'apps', label: 'Apps', icon: AppWindow},
  {key: 'jobs', label: 'Jobs', icon: Clock3},
  {key: 'access', label: 'Access', icon: UsersRound},
  {key: 'activity', label: 'Activity', icon: Activity},
] as const;

type PageKey = (typeof pages)[number]['key'];

function pageFromHash(): PageKey {
  const page = window.location.hash.replace(/^#\/?/, '').split('/')[0];
  return pages.some(item => item.key === page) ? (page as PageKey) : 'overview';
}

function usePage(): PageKey {
  return useSyncExternalStore(
    listener => {
      window.addEventListener('hashchange', listener);
      return () => window.removeEventListener('hashchange', listener);
    },
    pageFromHash,
  );
}

const ACTIVE = new Set(['awaiting_upload', 'queued', 'running']);

function hasActiveWork(dashboard: Dashboard): boolean {
  return [...dashboard.syncs, ...dashboard.deployments].some(item => ACTIVE.has(item.status));
}

function CreateTenant({onCreated}: {onCreated: (slug: string) => Promise<void>}) {
  const [busy, setBusy] = useState(false);
  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const slug = String(new FormData(event.currentTarget).get('slug')).trim();
    setBusy(true);
    try {
      await api.createTenant(slug);
      toast('Workspace created', `${slug} is ready.`);
      await onCreated(slug);
    } catch (error) {
      toast('Could not create tenant', (error as Error).message, 'error');
      setBusy(false);
    }
  };
  return (
    <main className="connect">
      <section>
        <Logo size={24} />
        <p className="kicker">Welcome to Oort</p>
        <h1>Create a tenant</h1>
        <p className="lede">A tenant is the secure boundary around datasets, queries, apps, and members.</p>
        <form onSubmit={submit}>
          <label>
            Tenant slug
            <input
              name="slug"
              required
              minLength={3}
              maxLength={63}
              pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]"
              placeholder="acme-labs"
              autoComplete="off"
            />
          </label>
          <p className="fine">Lowercase letters, numbers, and hyphens. It appears in your app URLs.</p>
          <button className="button" type="submit" disabled={busy}>
            {busy ? 'Creating…' : 'Create workspace'}
          </button>
        </form>
      </section>
    </main>
  );
}

export default function App() {
  const page = usePage();
  const [user, setUser] = useState<User | null>(null);
  const [checked, setChecked] = useState(false);
  const [notice, setNotice] = useState('');
  const [tenants, setTenants] = useState<Tenant[]>([]);
  const [dashboard, setDashboard] = useState<Dashboard | null>(null);

  const loadWorkspace = useCallback(async (preferred?: string) => {
    try {
      const listed = (await api.listTenants()).tenants || [];
      setTenants(listed);
      if (!listed.length) {
        setDashboard(null);
        return;
      }
      const remembered = preferred || localStorage.getItem('oort.tenant');
      const tenant = listed.find(item => item.slug === remembered) || listed[0];
      localStorage.setItem('oort.tenant', tenant.slug);
      setDashboard(await api.dashboard(tenant.slug));
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setUser(null);
        setNotice('Your session expired. Connect again to continue.');
      } else {
        toast('Workspace could not load', (error as Error).message, 'error');
      }
    }
  }, []);

  useEffect(() => {
    (async () => {
      try {
        setUser((await api.me()).user);
      } catch (error) {
        if (!(error instanceof ApiError && error.status === 401)) {
          setNotice((error as Error).message);
        }
      } finally {
        setChecked(true);
      }
    })();
  }, []);

  useEffect(() => {
    if (!user) return;
    const target = new URL(window.location.href);
    const invitedTenant = target.searchParams.get('tenant') || undefined;
    if (invitedTenant) {
      target.searchParams.delete('tenant');
      window.history.replaceState({}, '', target.pathname + target.search + target.hash);
    }
    loadWorkspace(invitedTenant);
  }, [user, loadWorkspace]);

  // Poll quietly while imports or deployments are in flight.
  useEffect(() => {
    if (!dashboard || !hasActiveWork(dashboard)) return;
    const timer = setTimeout(() => loadWorkspace(dashboard.tenant.slug), 2000);
    return () => clearTimeout(timer);
  }, [dashboard, loadWorkspace]);

  if (!checked) return null;
  if (!user) return <><Connect notice={notice} onConnected={setUser} /><Toasts /></>;
  if (checked && !dashboard && !tenants.length) {
    return <><CreateTenant onCreated={loadWorkspace} /><Toasts /></>;
  }
  if (!dashboard) return null;

  const reload = () => loadWorkspace(dashboard.tenant.slug);
  const view = {
    overview: <Overview dashboard={dashboard} />,
    datasets: <Datasets dashboard={dashboard} reload={reload} />,
    queries: <Queries key={dashboard.tenant.id} dashboard={dashboard} reload={reload} />,
    connectors: <Connectors key={dashboard.tenant.id} tenant={dashboard.tenant.slug} />,
    apps: <Apps dashboard={dashboard} reload={reload} />,
    jobs: <Jobs key={dashboard.tenant.id} tenant={dashboard.tenant.slug} />,
    access: <Access key={dashboard.tenant.id} tenant={dashboard.tenant} />,
    activity: <ActivityPage dashboard={dashboard} />,
  }[page];

  return (
    <div className="shell">
      <aside className="sidebar">
        <a className="brand" href="#/overview" title="Overview">
          <Logo size={18} />
          <span>Oort</span>
        </a>
        <nav aria-label="Primary">
          {pages.map(({key, label, icon: Icon}) => (
            <a key={key} href={`#/${key}`} title={label} className={page === key ? 'active' : ''} aria-current={page === key ? 'page' : undefined}>
              <Icon size={15} aria-hidden="true" />
              <span>{label}</span>
            </a>
          ))}
        </nav>
        <div className="sidebar-foot">
          <label className="tenant-select" title={`Tenant: ${dashboard.tenant.slug}`}>
            <span>Tenant</span>
            <select id="tenant" name="tenant" value={dashboard.tenant.slug} onChange={event => loadWorkspace(event.target.value)}>
              {tenants.map(tenant => <option key={tenant.id} value={tenant.slug}>{tenant.slug}</option>)}
            </select>
          </label>
          <div className="session">
            <span title={user.email}>{user.email.split('@')[0]}</span>
            <button className="bare-button" type="button" aria-label="Log out" title="Log out" onClick={async () => {
              try { await api.logout(); } finally { setUser(null); setDashboard(null); }
            }}><LogOut size={14} aria-hidden="true" /></button>
          </div>
        </div>
      </aside>
      <main>{view}</main>
      <Toasts />
    </div>
  );
}
