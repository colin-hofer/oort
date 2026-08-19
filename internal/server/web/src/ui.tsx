import {AlertTriangle, Check, Copy, type LucideIcon} from 'lucide-react';
import type {ReactNode} from 'react';
import {toast, useToasts} from './toast';

// The Oort mark: a scattered shell of icy bodies around a bright core.
// Colors come from the stylesheet so it follows the theme wherever it is placed.
export function Logo({size = 18}: {size?: number}) {
  return (
    <svg className="logo" viewBox="0 0 64 64" width={size} height={size} aria-hidden="true">
      <g fill="currentColor">
        <circle cx="56" cy="21" r="3" />
        <circle cx="46" cy="40" r="2.6" />
        <circle cx="14" cy="47" r="3.2" />
        <circle cx="9" cy="36" r="2.2" />
        <circle cx="20" cy="26" r="2.4" />
        <circle cx="42" cy="17" r="2.8" />
      </g>
      <circle className="core" cx="32" cy="32" r="6" />
    </svg>
  );
}

// Status values arrive from the API; anything unrecognized renders as neutral.
const statusTones: Record<string, string> = {
  succeeded: 'ok',
  running: 'busy',
  queued: 'busy',
  awaiting_upload: 'busy',
  failed: 'bad',
  cancelled: 'idle',
};

export function Status({value}: {value?: string | null}) {
  const status = value || 'unknown';
  return (
    <span className={`status status-${statusTones[status] || 'idle'}`}>
      <i aria-hidden="true" />
      {status.replaceAll('_', ' ')}
    </span>
  );
}

export function PageHead({title, children}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <header className="page-head">
      <h1>{title}</h1>
      {children && <div className="page-actions">{children}</div>}
    </header>
  );
}

export function Empty({icon: Icon, title, children}: {
  icon: LucideIcon;
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="empty">
      <Icon size={22} aria-hidden="true" />
      <h2>{title}</h2>
      {children}
    </div>
  );
}

export function Command({text}: {text: string}) {
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      toast('Copied', text);
    } catch {
      toast('Copy failed', 'Select the command and copy it manually.', 'error');
    }
  };
  return (
    <span className="command">
      <code>{text}</code>
      <button type="button" onClick={copy} aria-label={`Copy ${text}`}>
        <Copy size={13} aria-hidden="true" />
      </button>
    </span>
  );
}

export function Toasts() {
  const toasts = useToasts();
  return (
    <div className="toast-region" aria-live="assertive">
      {toasts.map(item => (
        <div key={item.id} className={`toast${item.kind === 'error' ? ' toast-error' : ''}`}>
          {item.kind === 'error'
            ? <AlertTriangle size={15} aria-hidden="true" />
            : <Check size={15} aria-hidden="true" />}
          <div>
            <strong>{item.title}</strong>
            {item.detail && <span>{item.detail}</span>}
          </div>
        </div>
      ))}
    </div>
  );
}
