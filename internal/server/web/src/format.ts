export function formatNumber(value: number): string {
  return new Intl.NumberFormat().format(value);
}

export function formatBytes(value?: number | null): string {
  if (value == null) return '—';
  const units = ['B', 'KB', 'MB', 'GB'];
  let amount = value;
  let index = 0;
  while (amount >= 1000 && index < units.length - 1) {
    amount /= 1000;
    index++;
  }
  return `${index && amount < 10 ? amount.toFixed(1) : Math.round(amount)} ${units[index]}`;
}

export function formatDate(value: string): string {
  return new Intl.DateTimeFormat(undefined, {dateStyle: 'medium', timeStyle: 'short'}).format(new Date(value));
}

export function relativeTime(value: string): string {
  const formatter = new Intl.RelativeTimeFormat(undefined, {numeric: 'auto'});
  let amount = Math.round((new Date(value).getTime() - Date.now()) / 1000);
  for (const [unit, next] of [['second', 60], ['minute', 60], ['hour', 24]] as const) {
    if (Math.abs(amount) < next) return formatter.format(amount, unit);
    amount = Math.round(amount / next);
  }
  return formatter.format(amount, 'day');
}

export function shortId(value?: string | null): string {
  return value ? value.slice(0, 8) : '—';
}

export function formatCell(value: unknown): string {
  if (value == null) return 'null';
  return typeof value === 'object' ? JSON.stringify(value) : String(value);
}

const actionLabels: Record<string, string> = {
  'tenant.created': 'Tenant created',
  'dataset.upload_created': 'Dataset upload created',
  'dataset.imported': 'Dataset snapshot published',
  'query.saved': 'Query revision saved',
  'deployment.upload_created': 'Deployment upload created',
  'deployment.published': 'App release published',
  'deployment.rolled_back': 'App release rolled back',
};

export function actionLabel(action: string): string {
  return actionLabels[action] || action.replaceAll('.', ': ').replaceAll('_', ' ');
}
