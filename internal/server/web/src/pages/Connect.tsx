import {ArrowRight} from 'lucide-react';
import {useState} from 'react';
import {api, type User} from '../api';
import {Logo} from '../ui';

export default function Connect({notice, onConnected}: {
  notice?: string;
  onConnected: (user: User) => void;
}) {
  const [problem, setProblem] = useState(notice || '');
  const [busy, setBusy] = useState(false);

  const submit = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const token = String(new FormData(event.currentTarget).get('token'));
    setBusy(true);
    try {
      const {user} = await api.connect(token);
      onConnected(user);
    } catch (error) {
      setProblem((error as Error).message);
      setBusy(false);
    }
  };

  return (
    <main className="connect">
      <section>
        <Logo size={24} />
        <p className="kicker">Oort control plane</p>
        <h1>Connect to your workspace</h1>
        <p className="lede">
          A scoped token becomes an HttpOnly browser session; it is never stored in page JavaScript.
        </p>
        {problem && <p className="problem" role="alert">{problem}</p>}
        <a className="button" href="/auth/login">Sign in with your identity provider <ArrowRight size={14} aria-hidden="true" /></a>
        <div className="divider"><span>or use an API token</span></div>
        <form onSubmit={submit}>
          <label>
            API token
            <input name="token" type="password" required autoComplete="off" placeholder="Paste your local token" />
          </label>
          <p className="fine">
            Local token: <code>jq -r .token ~/.local/state/oort/local.json</code>
          </p>
          <button className="button" type="submit" disabled={busy}>
            {busy ? 'Connecting…' : 'Connect'} <ArrowRight size={14} aria-hidden="true" />
          </button>
        </form>
      </section>
    </main>
  );
}
