WITH anchor AS (
  SELECT max(CAST(updated_at AS TIMESTAMP)) AS snapshot_at
  FROM "github-issues"
), events AS (
  SELECT CAST(created_at AS DATE) AS day, 1 AS opened, 0 AS closed
  FROM "github-issues"
  CROSS JOIN anchor
  WHERE CAST(created_at AS TIMESTAMP) >= anchor.snapshot_at - ($days * INTERVAL 1 DAY)
  UNION ALL
  SELECT CAST(closed_at AS DATE) AS day, 0 AS opened, 1 AS closed
  FROM "github-issues"
  CROSS JOIN anchor
  WHERE closed_at IS NOT NULL
    AND CAST(closed_at AS TIMESTAMP) >= anchor.snapshot_at - ($days * INTERVAL 1 DAY)
)
SELECT
  strftime(day, '%b %d') AS day,
  sum(opened) AS opened,
  sum(closed) AS closed
FROM events
GROUP BY day
ORDER BY day
