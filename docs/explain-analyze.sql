\echo === Q1 keyset showtimes WITH index
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON) SELECT id, starts_at FROM showtimes WHERE starts_at >= now() AND starts_at < now() + interval '14 days' AND (starts_at, id) > (now() + interval '3 days', 0) ORDER BY starts_at, id LIMIT 51;
BEGIN; DROP INDEX showtimes_starts_idx;
\echo === Q1 keyset showtimes WITHOUT index
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON) SELECT id, starts_at FROM showtimes WHERE starts_at >= now() AND starts_at < now() + interval '14 days' AND (starts_at, id) > (now() + interval '3 days', 0) ORDER BY starts_at, id LIMIT 51;
ROLLBACK;
\echo === Q2 sweeper expired holds WITH partial index
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON) SELECT showtime_id, seat_id FROM show_seats WHERE status = 'held' AND held_until <= now() ORDER BY held_until LIMIT 5000 FOR UPDATE SKIP LOCKED;
BEGIN; DROP INDEX show_seats_expiry_idx;
\echo === Q2 sweeper expired holds WITHOUT partial index
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON) SELECT showtime_id, seat_id FROM show_seats WHERE status = 'held' AND held_until <= now() ORDER BY held_until LIMIT 5000 FOR UPDATE SKIP LOCKED;
ROLLBACK;
\echo === Q3 seat map of one showtime (PK)
EXPLAIN (ANALYZE, COSTS OFF, SUMMARY ON) SELECT seat_id, status FROM show_seats WHERE showtime_id = 12;
