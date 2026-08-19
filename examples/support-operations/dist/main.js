import {createClient} from './oort-sdk.js';

const client = createClient();
const elements = {
  filters: document.querySelector('#filters'),
  window: document.querySelector('#window'),
  snapshot: document.querySelector('#snapshot'),
  error: document.querySelector('#error'),
  metrics: document.querySelector('.metrics'),
  backlog: document.querySelector('#backlog'),
  overdue: document.querySelector('#overdue'),
  age: document.querySelector('#age'),
  comments: document.querySelector('#comments'),
  queues: document.querySelector('#queues'),
  volume: document.querySelector('#volume'),
  queueFilter: document.querySelector('#queue-filter'),
  issues: document.querySelector('#issues'),
  empty: document.querySelector('#empty'),
};

function records(result) {
  return result.rows.map(row => Object.fromEntries(result.columns.map((column, index) => [column.name, row[index]])));
}

function cell(row, text, className = '') {
  const item = row.insertCell();
  item.textContent = text ?? '—';
  if (className) item.className = className;
  return item;
}

function labelledCell(row, primary, secondary, href = '') {
  const item = row.insertCell();
  const strong = document.createElement('span');
  const small = href ? document.createElement('a') : document.createElement('span');
  strong.className = primary.startsWith('#') ? 'ticket-id' : 'team-name';
  small.className = primary.startsWith('#') ? 'ticket-title' : 'team-lead';
  strong.textContent = primary;
  small.textContent = secondary;
  if (href) {
    small.href = href;
    small.target = '_blank';
    small.rel = 'noopener';
  }
  item.append(strong, small);
  return item;
}

function pill(value) {
  const item = document.createElement('span');
  item.className = `pill ${String(value).replaceAll('_', '-')}`;
  item.textContent = String(value).replaceAll('_', ' ');
  return item;
}

function renderSummary(summary) {
  elements.snapshot.textContent = `Dataset snapshot · ${summary.snapshot_label}`;
  elements.backlog.textContent = summary.active_backlog.toLocaleString();
  elements.overdue.textContent = summary.overdue.toLocaleString();
  elements.age.textContent = summary.median_open_age == null ? '—' : Math.round(summary.median_open_age).toLocaleString();
  elements.comments.textContent = summary.open_comments == null ? '0' : summary.open_comments.toLocaleString();
}

function renderQueues(queues) {
  elements.queues.replaceChildren();
  for (const queue of queues) {
    const row = elements.queues.insertRow();
    labelledCell(row, queue.queue, queue.owner);
    cell(row, queue.backlog.toLocaleString());
    cell(row, queue.overdue.toLocaleString(), queue.overdue ? 'bad-number' : '');
    cell(row, queue.average_age_hours == null ? '—' : `${queue.average_age_hours}h`);
    const load = row.insertCell();
    const track = document.createElement('span');
    const fill = document.createElement('i');
    track.className = 'load-track';
    fill.style.width = `${Math.min(Number(queue.capacity_load), 100)}%`;
    track.append(fill);
    load.append(track, `${queue.capacity_load}%`);
  }

  const selected = elements.queueFilter.value;
  elements.queueFilter.replaceChildren(new Option('All queues', 'all'));
  for (const queue of queues) elements.queueFilter.add(new Option(queue.queue[0].toUpperCase() + queue.queue.slice(1), queue.queue));
  elements.queueFilter.value = [...elements.queueFilter.options].some(option => option.value === selected) ? selected : 'all';
}

function renderVolume(volume) {
  elements.volume.replaceChildren();
  const maximum = Math.max(1, ...volume.map(day => Number(day.opened)));
  for (const day of volume) {
    const row = document.createElement('div');
    const bars = document.createElement('span');
    const created = document.createElement('i');
    const resolved = document.createElement('i');
    row.className = 'volume-row';
    bars.className = 'bars';
    created.className = 'bar created';
    resolved.className = 'bar resolved';
    created.style.width = `${100 * Number(day.opened) / maximum}%`;
    resolved.style.width = `${100 * Number(day.closed) / maximum}%`;
    bars.append(created, resolved);
    row.append(document.createTextNode(day.day), bars, document.createTextNode(day.opened));
    row.setAttribute('aria-label', `${day.day}: ${day.opened} opened, ${day.closed} closed`);
    elements.volume.append(row);
  }
}

function renderIssues(issues) {
  elements.issues.replaceChildren();
  elements.empty.hidden = issues.length > 0;
  for (const issue of issues) {
    const row = elements.issues.insertRow();
    labelledCell(row, `#${issue.number}`, issue.title, issue.html_url);
    labelledCell(row, issue.queue, issue.assignee);
    const activity = row.insertCell();
    activity.append(pill(issue.activity));
    cell(row, issue.comments.toLocaleString());
    cell(row, `${issue.age_hours}h`);
    const sla = row.insertCell();
    sla.append(pill(issue.sla_state));
  }
}

async function loadBoard() {
  const days = Number(elements.window.value);
  elements.error.hidden = true;
  elements.metrics.setAttribute('aria-busy', 'true');
  try {
    const [summaryResult, teamsResult, volumeResult] = await Promise.all([
      client.query('ops-summary', {days}),
      client.query('team-health', {days}),
      client.query('ticket-volume', {days}),
    ]);
    renderSummary(records(summaryResult)[0]);
    renderQueues(records(teamsResult));
    renderVolume(records(volumeResult));
    await loadTickets();
  } catch (error) {
    elements.error.textContent = `The operations board could not load. ${error.message} Check that both datasets have a published snapshot.`;
    elements.error.hidden = false;
  } finally {
    elements.metrics.setAttribute('aria-busy', 'false');
  }
}

async function loadTickets() {
  const result = await client.query('priority-backlog', {
    days: Number(elements.window.value),
    queue: elements.queueFilter.value,
    limit: 50,
  });
  renderIssues(records(result));
}

elements.filters.addEventListener('submit', event => {
  event.preventDefault();
  loadBoard();
});
elements.queueFilter.addEventListener('change', () => loadTickets().catch(error => {
  elements.error.textContent = `The backlog could not load. ${error.message}`;
  elements.error.hidden = false;
}));

loadBoard();
