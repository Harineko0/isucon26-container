select calls, round(total_exec_time::numeric,1) total_ms, round(mean_exec_time::numeric,2) mean_ms,
  left(regexp_replace(query, '\s+', ' ', 'g'), 120) q
from pg_stat_statements order by total_exec_time desc limit 25;
