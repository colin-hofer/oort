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
  WHERE state = 'open'
    AND CAST(updated_at AS TIMESTAMP) >= anchor.snapshot_at - ($days * INTERVAL 1 DAY)
)
SELECT
  number,
  title,
  html_url,
  classified.queue,
  coalesce(assignee.login, 'Unassigned') AS assignee,
  comments,
  CASE WHEN comments >= 10 THEN 'hot'
       WHEN comments >= 3 THEN 'active'
       ELSE 'quiet' END AS activity,
  age_hours,
  CASE
    WHEN assignee IS NOT NULL THEN 'assigned'
    WHEN age_hours > policies.sla_hours THEN 'overdue'
    WHEN age_hours >= policies.sla_hours * 0.75 THEN 'at_risk'
    ELSE 'on_track'
  END AS sla_state
FROM classified
JOIN "triage-policies" AS policies USING (queue)
WHERE ($queue = 'all' OR classified.queue = $queue)
ORDER BY
  CASE WHEN assignee IS NULL AND age_hours > policies.sla_hours THEN 0
       WHEN assignee IS NULL AND age_hours >= policies.sla_hours * 0.75 THEN 1
       ELSE 2 END,
  comments DESC,
  age_hours DESC
LIMIT $limit
