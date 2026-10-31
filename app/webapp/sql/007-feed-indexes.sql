-- 「当日に投稿済みか」の判定（フィードの viewer CTE、loadViewerContext、isureal_post_access、
-- isureal_create_post の件数確認）は「現在の期間 → 同じ local_date の期間（全リージョン）→ posts」の順に引く。
-- posts 側は posts_author_window (author_id, posting_window_id) で引ける（002-indexes.sql、#77）。
-- posting_windows 側は local_date だけの条件なので、(region, local_date) の一意索引を全件なめていた（約 0.1ms/回）。
-- posting_windows は千数百行なので作成は一瞬。
-- 月別 Memories は日付範囲内の全期間の ID・期限・終了時刻を読む。過去の期間は更新されないため、
-- これらを索引に含めると可視性マップが立っている期間は heap を読まずに済む。
CREATE INDEX IF NOT EXISTS posting_windows_date_details ON posting_windows (local_date) INCLUDE (id, on_time_deadline, ended_at);
DROP INDEX IF EXISTS posting_windows_local_date;
