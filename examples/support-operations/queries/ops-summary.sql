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
    date_diff('hour', CAST(created_at AS TIMESTAMP), anchor.snapshot_at) AS age_hours,
    anchor.snapshot_at
  FROM "github-issues" AS issues
  CROSS JOIN anchor
  WHERE CAST(updated_at AS TIMESTAMP) >= anchor.snapshot_at - ($days * INTERVAL 1 DAY)
), scoped AS (
  SELECT classified.*, policies.sla_hours
  FROM classified
  JOIN "triage-policies" AS policies USING (queue)
)
SELECT
  strftime(max(snapshot_at), '%b %d, %Y · %H:%M UTC') AS snapshot_label,
  count(*) FILTER (WHERE state = 'open') AS active_backlog,
  count(*) FILTER (
    WHERE state = 'open' AND assignee IS NULL AND age_hours > sla_hours
  ) AS overdue,
  round(median(age_hours) FILTER (WHERE state = 'open'), 0) AS median_open_age,
  sum(comments) FILTER (WHERE state = 'open') AS open_comments
FROM scoped
