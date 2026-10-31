#!/usr/bin/env python3
"""PreToolUse hook: エージェントがベンチを汚す操作をしないよう止める。

止めるもの: 手元からのデプロイ・スモーク、サーバのプロセス・設定・ファイルの変更、DB の書き込み・
スキーマ変更、ルーター経由の書き込み API、ポータルからのベンチ投入。デプロイとベンチは CI だけが行う。
通すもの: ssh での読み取り（journalctl, ps, cat, top 等）、psql の SELECT / EXPLAIN、infra/deploy.sh -n。
"""
import json
import re
import sys

SERVERS = r"13\.159\.52\.104|52\.199\.206\.161|52\.197\.40\.86|3\.115\.193\.33|192\.168\.[1-4]\.10|isucon26-(asia|europe|america)"
ROUTER = r"35\.73\.68\.236"
# psql を実際に起動しているとき（文書中の「psql」という語には反応しない）
PSQL = r"\bpsql\b[^\n]*\s-[cfhdU]\b|\bpsql\b[^\n]*<|\|\s*psql\b|PGPASSWORD"
CMD_START = r"(^|[;&|(]\s*|(?<![\w./-])(bash|sh|zsh|exec|time|nohup)\s+)([A-Z_]+=\S*\s+)*"

# asia2 で重い読み取りをすると、走っているベンチの計測 (スコア、pg_stat_*、shared_buffers の中身) が汚れる (#84)。
PG_LOG = r"postgresql-16-main\.log|/var/log/postgresql"
BIG_TABLES = r"(realmojis|notifications|comments|posts|image_objects|users|friendships|login_sessions)"

RULES = [
    # (対象, パターン, 理由)
    (SERVERS, r"(^|[\s;&|(])(grep|egrep|zgrep|rg|ag|less|more|cat|awk|sed|wc|sort|tac)\s[^|;&]*(" + PG_LOG + ")",
     "PostgreSQL のログ (数 GB) を全部読むとベンチ中の asia2 の I/O とページキャッシュを汚す (#84)。tail -n で末尾だけ読む"),
    (PSQL, r"\bcount\s*\((?![^;]*\bwhere\b)[^;]*\bfrom\s+" + BIG_TABLES + r"\b"
           r"|\bfrom\s+" + BIG_TABLES + r"\b(?![^;]*\bwhere\b)[^;]*\b(group\s+by|order\s+by|distinct)\b",
     "大きい表の全件 count / GROUP BY / ORDER BY はベンチ中の asia2 を汚す (#84)。件数は pg_stat_user_tables.n_live_tup か "
     "pg_class.reltuples、計画は EXPLAIN (ANALYZE なし) で見る。WHERE で絞るか、ベンチが走っていないときに人が流す"),
    (None, r"ALLOW_LOCAL_DEPLOY", "ALLOW_LOCAL_DEPLOY は人間が緊急時に使うもの"),
    (None, CMD_START + r"\S*(?<![\w-])(deploy|smoke)\.sh(?!\s+-n\b)",
     "deploy.sh / smoke.sh は CI から実行する（infra/deploy.sh -n の差分表示は可）"),
    (None, r"portal\.py\s+enqueue", "ポータルへのベンチ投入"),
    (r"portal\.isucon\.net", r"-X\s*POST|--data|\s-d\s|enqueue", "ポータルへのベンチ投入"),
    (SERVERS, r"(^|[\s;&|(])(rsync|scp|sftp)\s", "サーバへのファイル転送"),
    (SERVERS, r"systemctl\s+(restart|reload|stop|start|enable|disable|daemon-reload|kill|edit)\b"
              r"|\bservice\s+\S+\s+(restart|reload|stop|start)\b|nginx\s+-s\b|\breboot\b|\bshutdown\b"
              r"|\bp?kill(all)?\b|\bsysctl\s+(-w|--system|-p)\b|init\.sh|\bgo\s+build\b|\bmake\b",
     "サーバのプロセス・設定の変更"),
    (SERVERS, r"sudo\s+(-\S+\s+)*(tee|cp|mv|rm|install|ln|chmod|chown|truncate|dd)\b|\bsed\s+-i|(?<![0-9&])>>?\s*[/~$]",
     "サーバ上のファイルの書き換え"),
    (PSQL, r"\b(insert|update|delete|truncate|create|drop|alter|vacuum|reindex|cluster|grant|revoke|comment\s+on"
                  r"|refresh|call|pg_reload_conf|pg_terminate_backend|pg_cancel_backend|pg_stat_statements_reset|pg_stat_reset)\b"
                  r"|\bcopy\s+\S+\s+from\b|\bset\s+(session\s+|local\s+)?\w+\s*(=|to)\b|\s-f\s|<",
     "DB の書き込み・スキーマ変更（索引や関数は sql/ に書いて CI で反映する）"),
    (ROUTER, r"-X\s*(POST|PUT|PATCH|DELETE)|--request\s+(POST|PUT|PATCH|DELETE)"
             r"|(^|\s)(-d|-F|--data\S*|--form|-T|--upload-file)(\s|=)|/api/initialize",
     "ルーター経由の書き込み API（走行中のベンチを壊す）"),
]


def deny(reason):
    print(f"BLOCKED: {reason}\n"
          "デプロイ・ベンチは CI だけが行う。PR に `/bench [メモ]` とコメントする。詳細は ci/README.md。",
          file=sys.stderr)
    sys.exit(2)


def check_bash(cmd):
    # /dev/null へのリダイレクトは書き込みに数えない
    cmd = re.sub(r"[0-9&]?>>?\s*/dev/null", "", cmd)
    for scope, pat, reason in RULES:
        if scope and not re.search(scope, cmd):
            continue
        if re.search(pat, cmd, re.I if scope == PSQL else 0):
            deny(reason)


def main():
    data = json.load(sys.stdin)
    tool, inp = data.get("tool_name", ""), data.get("tool_input", {})
    if tool == "Bash":
        check_bash(inp.get("command", ""))
    elif tool.endswith("__navigate"):
        if re.search(r"portal\.isucon\.net/contestant/benchmark_jobs/?($|[?#])", inp.get("url", "")):
            deny("ポータルのベンチ一覧（Enqueue ボタンがある）。結果は /contestant/benchmark_jobs/<id> か PR コメントで見る")
    elif tool.endswith("__javascript_tool"):
        js = inp.get("text", "")
        if "benchmark_jobs" in js and re.search(r"post|enqueue", js, re.I):
            deny("ポータルへのベンチ投入")


if __name__ == "__main__":
    main()
