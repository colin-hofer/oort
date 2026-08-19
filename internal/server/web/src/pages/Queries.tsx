import {Play, Plus, Save, Trash2} from 'lucide-react';
import {useMemo, useState} from 'react';
import {api, type Dashboard, type QueryResult} from '../api';
import {formatCell, formatNumber, relativeTime} from '../format';
import {SqlEditor, type SqlSchema} from '../sql-editor';
import {toast} from '../toast';
import {PageHead} from '../ui';

function defaultParameters(types: Record<string, string>): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(types).map(([name, type]) => [
      name,
      type === 'boolean' ? false : type === 'string' ? '' : 50,
    ]),
  );
}

type Draft = {name: string; sql: string; parameters: string};

function draftFor(dashboard: Dashboard, slug: string | null): Draft {
  const saved = dashboard.queries.find(query => query.slug === slug);
  if (saved) {
    return {
      name: saved.slug,
      sql: saved.sql,
      parameters: JSON.stringify(defaultParameters(saved.parameter_types)),
    };
  }
  const table = dashboard.datasets[0]?.slug;
  return {
    name: '',
    sql: table ? `SELECT *\nFROM ${table}\nLIMIT $limit` : 'SELECT 1 AS ready',
    parameters: JSON.stringify(table ? {limit: 50} : {}),
  };
}

function ResultTable({result}: {result: QueryResult}) {
  return (
    <div className="result">
      <div className="result-head">
        <strong>
          {formatNumber(result.rows.length)} rows{result.truncated ? ' · truncated' : ''}
        </strong>
        <span className="fine">snapshot #{result.snapshot_id}</span>
      </div>
      <div className="table-wrap result-scroll">
        <table>
          <thead>
            <tr>
              {result.columns.map(column => (
                <th key={column.name}>
                  {column.name} <span className="fine">{column.type}</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {result.rows.map((row, index) => (
              <tr key={index}>
                {row.map((value, column) => <td key={column}>{formatCell(value)}</td>)}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export default function Queries({dashboard, reload}: {dashboard: Dashboard; reload: () => Promise<void>}) {
  const initial = dashboard.queries[0]?.slug ?? null;
  const [selected, setSelected] = useState<string | null>(initial);
  const [draft, setDraft] = useState<Draft>(() => draftFor(dashboard, initial));
  const [result, setResult] = useState<QueryResult | null>(null);
  const [running, setRunning] = useState(false);

  const select = (slug: string | null) => {
    setSelected(slug);
    setDraft(draftFor(dashboard, slug));
    setResult(null);
  };

  const run = async () => {
    let parameters: Record<string, unknown>;
    try {
      parameters = JSON.parse(draft.parameters || '{}');
    } catch {
      toast('Parameters are not valid JSON', 'Use an object such as {"limit": 50}.', 'error');
      return;
    }
    setRunning(true);
    try {
      const response = await api.executeDraftQuery(dashboard.tenant.slug, draft.sql, parameters);
      setResult(response.result);
      toast('Query completed', `${response.result.rows.length} rows returned.`);
    } catch (error) {
      toast('Query failed', (error as Error).message, 'error');
    } finally {
      setRunning(false);
    }
  };

  const save = async () => {
    const name = draft.name.trim();
    if (!/^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/.test(name)) {
      toast('Query name is invalid', 'Use 3–63 lowercase letters, numbers, or hyphens.', 'error');
      return;
    }
    let parameters: Record<string, unknown>;
    try {
      parameters = JSON.parse(draft.parameters || '{}');
    } catch {
      toast('Parameters are not valid JSON', 'Use an object such as {"limit": 50}.', 'error');
      return;
    }
    setRunning(true);
    try {
      const {query} = await api.saveQuery(dashboard.tenant.slug, name, draft.sql, parameters);
      setSelected(query.slug);
      toast('Query saved', `${query.slug} revision ${query.version} is ready to deploy.`);
      await reload();
    } catch (error) {
      toast('Query could not be saved', (error as Error).message, 'error');
    } finally {
      setRunning(false);
    }
  };

  const remove = async () => {
    if (!selected || !window.confirm(`Delete query ${selected} and all of its revisions?`)) return;
    setRunning(true);
    try {
      await api.deleteQuery(dashboard.tenant.slug, selected);
      const remaining = {...dashboard, queries: dashboard.queries.filter(query => query.slug !== selected)};
      setSelected(null);
      setDraft(draftFor(remaining, null));
      setResult(null);
      await reload();
      toast('Query deleted', selected);
    } catch (error) {
      toast('Query could not be deleted', (error as Error).message, 'error');
    } finally {
      setRunning(false);
    }
  };

  const selectedQuery = dashboard.queries.find(query => query.slug === selected);
  const schema = useMemo<SqlSchema>(
    () => Object.fromEntries(dashboard.datasets.map(dataset => [dataset.slug, (dataset.schema || []).map(column => column.name)])),
    [dashboard.datasets],
  );
  return (
    <>
      <PageHead title="Queries" />
      <div className="workbench">
        <aside className="workbench-side">
          <div className="query-list">
            <div className="query-list-head">
              <span>Saved</span>
              <button type="button" className="icon-button" onClick={() => select(null)} aria-label="New query">
                <Plus size={14} aria-hidden="true" />
              </button>
            </div>
            {dashboard.queries.map(query => (
              <button
                key={query.id}
                type="button"
                className={`query-tab${query.slug === selected ? ' active' : ''}`}
                onClick={() => select(query.slug)}
              >
                <strong>{query.slug}</strong>
                <span>revision {query.version}</span>
              </button>
            ))}
            {!dashboard.queries.length && <p className="fine">No saved queries.</p>}
          </div>
          <div className="schema-list">
            <div className="query-list-head"><span>Tables</span></div>
            {dashboard.datasets.map(dataset => (
              <details key={dataset.id} className="schema-table">
                <summary>{dataset.slug}</summary>
                {dataset.schema?.length
                  ? (
                    <ul className="schema-cols">
                      {dataset.schema.map(column => (
                        <li key={column.name}>
                          <code>{column.name}</code>
                          <span>{column.type.toLowerCase()}</span>
                        </li>
                      ))}
                    </ul>
                  )
                  : <p className="fine">No schema until the first import succeeds.</p>}
              </details>
            ))}
            {!dashboard.datasets.length && <p className="fine">No datasets yet.</p>}
          </div>
        </aside>
        <form className="editor" onSubmit={event => { event.preventDefault(); void run(); }}>
          <div className="editor-toolbar">
            <input
              className="query-name"
              id="query-name"
              name="name"
              value={draft.name}
              onChange={event => setDraft({...draft, name: event.target.value})}
              placeholder="query-name"
              aria-label="Query name"
              minLength={3}
              maxLength={63}
              pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]"
            />
            <span className="fine">
              {selectedQuery
                ? `revision ${selectedQuery.version} · ${relativeTime(selectedQuery.created_at)}`
                : 'unsaved draft'}
            </span>
          </div>
          <SqlEditor
            value={draft.sql}
            schema={schema}
            onChange={sql => setDraft(current => ({...current, sql}))}
            onRun={run}
          />
          <div className="editor-run">
            <label>
              Parameters
              <input
                id="query-parameters"
                name="parameters"
                value={draft.parameters}
                onChange={event => setDraft({...draft, parameters: event.target.value})}
                aria-label="Query parameters as JSON"
              />
            </label>
            <button className="button" type="submit" disabled={running}>
              <Play size={14} aria-hidden="true" /> {running ? 'Running…' : 'Run'}
            </button>
            <button className="button ghost" type="button" disabled={running} onClick={save}>
              <Save size={14} aria-hidden="true" /> Save revision
            </button>
            {selectedQuery && (
              <button className="icon-button" type="button" disabled={running} onClick={remove} aria-label={`Delete ${selectedQuery.slug}`}>
                <Trash2 size={14} aria-hidden="true" />
              </button>
            )}
          </div>
          {result
            ? <ResultTable result={result} />
            : <p className="result-hint">Run to inspect columns, rows, and the snapshot. Ctrl/⌘ + Enter runs; parameters use <code>$name</code> in SQL.</p>}
        </form>
      </div>
    </>
  );
}
