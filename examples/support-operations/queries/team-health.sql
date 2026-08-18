WITH anchor AS (
  SELECT max(CAST(updated_at AS TIMESTAMP)) AS snapshot_at
  FROM "github-issues"
), classified AS (
  SELECT
    issues.*,
    CASE
      WHEN list_contains(list_transform(labels, item -> lower(item.name)), 'needs triage') THEN 'triage'
      WHEN list_contains(list_transform(labels, item -> lower(item.name)), 'reproduced') THEN 'reproduced'
      ELSE 'general'
    END AS queue,
    date_diff('hour', CAST(created_at AS TIMESTAMP), anchor.snapshot_at) AS age_hours
  FROM "github-issues" AS issues
  CROSS JOIN anchor
  WHERE CAST(updated_at AS TIMESTAMP) >= anchor.snapshot_at - ($days * INTERVAL 1 DAY)
)
SELECT
  classified.queue,
  policies.owner,
  count(*) FILTER (WHERE state = 'open') AS backlog,
  count(*) FILTER (
    WHERE state = 'open' AND assignee IS NULL AND age_hours > policies.sla_hours
  ) AS overdue,
  round(avg(age_hours) FILTER (WHERE state = 'open'), 1) AS average_age_hours,
  policies.sla_hours,
  policies.daily_capacity,
  round(100.0 * count(*) FILTER (WHERE state = 'open') / policies.daily_capacity, 0) AS capacity_load
FROM classified
JOIN "triage-policies" AS policies USING (queue)
GROUP BY classified.queue, policies.owner, policies.sla_hours, policies.daily_capacity
ORDER BY overdue DESC, backlog DESC, classified.queue
