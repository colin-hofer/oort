import {Activity as ActivityIcon} from 'lucide-react';
import type {Dashboard} from '../api';
import {actionLabel, formatDate, relativeTime, shortId} from '../format';
import {Empty, PageHead} from '../ui';

export default function Activity({dashboard}: {dashboard: Dashboard}) {
  const events = dashboard.activity;
  return (
    <>
      <PageHead
        kicker="Inspectable history"
        title="Activity"
        lede="Recent mutations in this tenant, with the IDs needed to trace every request."
      />
      {events.length ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr><th>Action</th><th>Resource</th><th>Request</th><th>When</th></tr>
            </thead>
            <tbody>
              {events.map(event => (
                <tr key={event.id}>
                  <td className="name">{actionLabel(event.action)}</td>
                  <td>
                    {event.resource_type} <code className="id">{shortId(event.resource_id)}</code>
                  </td>
                  <td><code className="id">{event.request_id}</code></td>
                  <td title={formatDate(event.created_at)}>{relativeTime(event.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={ActivityIcon} title="Nothing has changed yet">
          <p>Create a tenant resource and its audit trail appears here.</p>
        </Empty>
      )}
    </>
  );
}
