import {useSyncExternalStore} from 'react';

export type Toast = {id: number; title: string; detail?: string; kind: 'ok' | 'error'};

let toasts: Toast[] = [];
let nextId = 1;
const listeners = new Set<() => void>();

function publish(next: Toast[]) {
  toasts = next;
  for (const listener of listeners) listener();
}

export function toast(title: string, detail = '', kind: Toast['kind'] = 'ok') {
  const entry = {id: nextId++, title, detail, kind};
  publish([...toasts, entry]);
  setTimeout(() => publish(toasts.filter(item => item.id !== entry.id)), 4500);
}

export function useToasts(): Toast[] {
  return useSyncExternalStore(
    listener => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    () => toasts,
  );
}
